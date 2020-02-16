package scanner

import (
	"container/list"
	"database/sql"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/h2non/filetype"
	"github.com/viktorstrate/photoview/api/graphql/models"
)

type scanner_cache map[string]interface{}

func (cache *scanner_cache) insert_photo_type(path string, content_type string) {
	(*cache)["photo_type//"+path] = content_type
}

func (cache *scanner_cache) get_photo_type(path string) *string {
	result, found := (*cache)["photo_type//"+path].(string)
	if found {
		// log.Printf("Image cache hit: %s\n", path)
		return &result
	}

	return nil
}

// Insert single album directory in cache
func (cache *scanner_cache) insert_album_path(path string, contains_photo bool) {
	(*cache)["album_path//"+path] = contains_photo
}

// Insert album path and all parent directories up to the given root directory in cache
func (cache *scanner_cache) insert_album_paths(end_path string, root string, contains_photo bool) {
	curr_path := path.Clean(end_path)
	root_path := path.Clean(root)

	for curr_path != root_path || curr_path == "." {

		cache.insert_album_path(curr_path, contains_photo)

		curr_path = path.Dir(curr_path)
	}
}

func (cache *scanner_cache) album_contains_photo(path string) *bool {
	contains_photo, found := (*cache)["album_path//"+path].(bool)
	if found {
		// log.Printf("Album cache hit: %s\n", path)
		return &contains_photo
	}

	return nil
}

func ScanUser(database *sql.DB, userId int) error {

	row := database.QueryRow("SELECT * FROM user WHERE user_id = ?", userId)
	user, err := models.NewUserFromRow(row)
	if err != nil {
		log.Printf("Could not find user to scan: %s\n", err.Error())
		return err
	}

	log.Printf("Starting scan for user '%s'\n", user.Username)
	go scan(database, user)

	return nil
}

func scan(database *sql.DB, user *models.User) {
	// Start scanning
	scanner_cache := make(scanner_cache)
	album_paths_scanned := make([]interface{}, 0)

	type scanInfo struct {
		path     string
		parentId *int
	}

	scanQueue := list.New()
	scanQueue.PushBack(scanInfo{
		path:     user.RootPath,
		parentId: nil,
	})

	for scanQueue.Front() != nil {
		albumInfo := scanQueue.Front().Value.(scanInfo)
		scanQueue.Remove(scanQueue.Front())

		albumPath := albumInfo.path
		albumParentId := albumInfo.parentId

		album_paths_scanned = append(album_paths_scanned, albumPath)

		// Read path
		dirContent, err := ioutil.ReadDir(albumPath)
		if err != nil {
			log.Printf("Could not read directory: %s\n", err.Error())
			return
		}

		tx, err := database.Begin()
		if err != nil {
			log.Printf("ERROR: Could not begin database transaction: %s\n", err)
			return
		}

		log.Printf("Scanning directory: %s", albumPath)

		// Make album if not exists
		albumTitle := path.Base(albumPath)
		_, err = tx.Exec("INSERT IGNORE INTO album (title, parent_album, owner_id, path) VALUES (?, ?, ?, ?)", albumTitle, albumParentId, user.UserID, albumPath)
		if err != nil {
			fmt.Printf("ERROR: Could not insert album into database: %s\n", err)
			tx.Rollback()
			return
		}

		row := tx.QueryRow("SELECT album_id FROM album WHERE path = ?", albumPath)
		var albumId int
		if err := row.Scan(&albumId); err != nil {
			fmt.Printf("ERROR: Could not get id of album: %s\n", err)
			tx.Rollback()
			return
		}

		// Commit album transaction
		if err := tx.Commit(); err != nil {
			log.Printf("ERROR: Could not commit database transaction: %s\n", err)
			return
		}

		// Scan for photos
		for _, item := range dirContent {
			photoPath := path.Join(albumPath, item.Name())

			if !item.IsDir() && isPathImage(photoPath, &scanner_cache) {
				tx, err := database.Begin()
				if err != nil {
					log.Printf("ERROR: Could not begin database transaction for image %s: %s\n", photoPath, err)
					return
				}

				content_type := scanner_cache.get_photo_type(photoPath)
				if content_type == nil {
					log.Println("Content type not found from cache")
					return
				}

				if err := ProcessImage(tx, photoPath, albumId, *content_type); err != nil {
					log.Printf("ERROR: processing image %s: %s", photoPath, err)
					tx.Rollback()
					return
				}

				tx.Commit()
			}
		}

		// Scan for sub-albums
		for _, item := range dirContent {
			subalbumPath := path.Join(albumPath, item.Name())

			if item.IsDir() && directoryContainsPhotos(subalbumPath, &scanner_cache) {
				scanQueue.PushBack(scanInfo{
					path:     subalbumPath,
					parentId: &albumId,
				})
			}
		}
	}

	cleanupCache(database, album_paths_scanned, user)

	log.Println("Done scanning")
}

func directoryContainsPhotos(rootPath string, cache *scanner_cache) bool {

	if contains_image := cache.album_contains_photo(rootPath); contains_image != nil {
		return *contains_image
	}

	scanQueue := list.New()
	scanQueue.PushBack(rootPath)

	scanned_directories := make([]string, 0)

	for scanQueue.Front() != nil {

		dirPath := scanQueue.Front().Value.(string)
		scanQueue.Remove(scanQueue.Front())

		scanned_directories = append(scanned_directories, dirPath)

		dirContent, err := ioutil.ReadDir(dirPath)
		if err != nil {
			log.Printf("Could not read directory: %s\n", err.Error())
			return false
		}

		for _, fileInfo := range dirContent {
			filePath := path.Join(dirPath, fileInfo.Name())
			if fileInfo.IsDir() {
				scanQueue.PushBack(filePath)
			} else {
				if isPathImage(filePath, cache) {
					cache.insert_album_paths(dirPath, rootPath, true)
					return true
				}
			}
		}

	}

	for _, scanned_path := range scanned_directories {
		cache.insert_album_path(scanned_path, false)
	}
	return false
}

var SupportedMimetypes = [...]string{
	"image/jpeg",
	"image/png",
	"image/tiff",
	"image/webp",
	"image/x-canon-cr2",
	"image/bmp",
}

var WebMimetypes = [...]string{
	"image/jpeg",
	"image/png",
	"image/webp",
	"image/bmp",
}

func isPathImage(path string, cache *scanner_cache) bool {
	if cache.get_photo_type(path) != nil {
		return true
	}
	file, err := os.Open(path)
	if err != nil {
		log.Printf("Could not open file %s: %s\n", path, err)
		return false
	}
	defer file.Close()

	head := make([]byte, 261)
	if _, err := file.Read(head); err != nil {
		log.Printf("Could not read file %s: %s\n", path, err)
		return false
	}

	imgType, err := filetype.Image(head)
	if err != nil {
		return false
	}

	for _, supported_mime := range SupportedMimetypes {
		if supported_mime == imgType.MIME.Value {
			cache.insert_photo_type(path, supported_mime)
			return true
		}
	}

	log.Printf("Unsupported image %s of type %s\n", path, imgType.MIME.Value)
	return false
}

func cleanupCache(database *sql.DB, scanned_albums []interface{}, user *models.User) {
	if len(scanned_albums) == 0 {
		return
	}

	args := make([]interface{}, 0)
	args = append(args, user.UserID)
	args = append(args, scanned_albums...)

	albums_questions := strings.Repeat("?,", len(scanned_albums))[:len(scanned_albums)*2-1]
	rows, err := database.Query("SELECT album_id FROM album WHERE album.owner_id = ? AND path NOT IN ("+albums_questions+")", args...)
	if err != nil {
		log.Printf("ERROR: Could not get albums from database: %s\n", err)
		return
	}

	deleted_albums := 0
	deleted_ids := make([]interface{}, 0)
	for rows.Next() {
		var album_id int
		rows.Scan(&album_id)
		deleted_ids = append(deleted_ids, album_id)
		cache_path := path.Join("./image-cache", strconv.Itoa(album_id))
		err := os.RemoveAll(cache_path)
		if err != nil {
			log.Printf("ERROR: Could not delete unused cache folder: %s\n%s\n", cache_path, err)
		} else {
			deleted_albums++
		}
	}

	if len(deleted_ids) > 0 {
		albums_questions = strings.Repeat("?,", len(deleted_ids))[:len(deleted_ids)*2-1]
		if _, err := database.Exec("DELETE FROM album WHERE album_id IN ("+albums_questions+")", deleted_ids...); err != nil {
			log.Printf("ERROR: Could not delete old albums from database:\n%s\n", err)
		}
	}

	log.Printf("Deleted %d unused albums from cache", deleted_albums)
}