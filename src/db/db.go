package db

import (
	"database/sql"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Nerdmaster/magicsql"
	_ "github.com/mattn/go-sqlite3" // database/sql requires "side-effect" packages be loaded
	"github.com/uoregon-libraries/gopkg/logger"
)

// Database encapsulates the database handle and magicsql table definitions
type Database struct {
	dbh           *magicsql.DB
	mtFiles       *magicsql.MagicTable
	mtFolders     *magicsql.MagicTable
	mtRealFolders *magicsql.MagicTable
	mtCategories  *magicsql.MagicTable
	mtInventories *magicsql.MagicTable
	mtArchiveJobs *magicsql.MagicTable
}

// Operation wraps a magicsql Operation with preloaded OperationTable
// definitions for easy querying
type Operation struct {
	Operation   *magicsql.Operation
	Files       *magicsql.OperationTable
	Folders     *magicsql.OperationTable
	RealFolders *magicsql.OperationTable
	Inventories *magicsql.OperationTable
	Categories  *magicsql.OperationTable
	ArchiveJobs *magicsql.OperationTable
}

// New sets up a database connection and returns a usable Database
func New() *Database {
	var _db, err = sql.Open("sqlite3", "db/da.db")
	if err != nil {
		logger.Fatalf("Unable to open database: %s", err)
	}

	return &Database{
		dbh:           magicsql.Wrap(_db),
		mtFiles:       magicsql.Table("files", &File{}),
		mtFolders:     magicsql.Table("folders", &Folder{}),
		mtRealFolders: magicsql.Table("real_folders", &RealFolder{}),
		mtCategories:  magicsql.Table("categories", &Category{}),
		mtInventories: magicsql.Table("inventories", &Inventory{}),
		mtArchiveJobs: magicsql.Table("archive_jobs", &ArchiveJob{}),
	}
}

// Operation returns a pre-set Operation for quick tasks that don't warrant a transaction
func (db *Database) Operation() *Operation {
	var magicOp = db.dbh.Operation()
	return &Operation{
		Operation:   magicOp,
		Files:       magicOp.OperationTable(db.mtFiles),
		Folders:     magicOp.OperationTable(db.mtFolders),
		RealFolders: magicOp.OperationTable(db.mtRealFolders),
		Inventories: magicOp.OperationTable(db.mtInventories),
		Categories:  magicOp.OperationTable(db.mtCategories),
		ArchiveJobs: magicOp.OperationTable(db.mtArchiveJobs),
	}
}

// InTransaction connects to the database and starts a transaction, used by all
// other Database calls, runs the callback function, then ends the transaction,
// returning the error (if any occurs)
func (db *Database) InTransaction(cb func(*Operation) error) error {
	var op = db.Operation()
	op.Operation.BeginTransaction()
	var err = cb(op)

	// Make sure we absolutely rollback if an error is returned
	if err != nil {
		op.Operation.Rollback()
		return err
	}

	op.Operation.EndTransaction()
	err = op.Operation.Err()
	if err != nil {
		return fmt.Errorf("database error: %s", err)
	}
	return nil
}

// AllInventories returns all the inventory files which have been indexed
func (op *Operation) AllInventories() ([]*Inventory, error) {
	var inventories []*Inventory
	op.Inventories.Select().AllObjects(&inventories)
	return inventories, op.Operation.Err()
}

// WriteInventory stores the given inventory object in the database
func (op *Operation) WriteInventory(i *Inventory) error {
	op.Inventories.Save(i)
	return op.Operation.Err()
}

// AllCategories returns all categories which have been seen
func (op *Operation) AllCategories() ([]*Category, error) {
	var categories []*Category
	op.Categories.Select().Order("LOWER(name)").AllObjects(&categories)
	return categories, op.Operation.Err()
}

// FindCategoryByName returns a category if one exists with the given name, and
// the database error if any occurred
func (op *Operation) FindCategoryByName(name string) (*Category, error) {
	var category = &Category{}
	var ok = op.Categories.Select().Where("name = ?", name).First(category)
	if !ok {
		category = nil
	}
	return category, op.Operation.Err()
}

// FindOrCreateCategory stores (or finds) the category by the given name and
// returns it.  If there are any database errors, they're returned and Category
// will be undefined.
func (op *Operation) FindOrCreateCategory(name string) (*Category, error) {
	var category, err = op.FindCategoryByName(name)
	if category == nil && err == nil {
		category = &Category{Name: name}
		op.Categories.Save(category)
	}
	return category, op.Operation.Err()
}

// FindFolderByPath looks for a folder with the given path under the given category
func (op *Operation) FindFolderByPath(c *Category, path string) (*Folder, error) {
	var folder = &Folder{}
	var ok = op.Folders.Select().Where("category_id = ? AND public_path = ?", c.ID, path).First(folder)
	if !ok {
		folder = nil
	}
	return folder, op.Operation.Err()
}

// FindOrCreateFolder centralizes the creation and DB-save operation for folders
func (op *Operation) FindOrCreateFolder(c *Category, f *Folder, path string) (*Folder, error) {
	var parentFolderID = 0
	if f != nil {
		parentFolderID = f.ID
	}
	var folder, err = op.FindFolderByPath(c, path)
	if err != nil {
		return nil, err
	}
	if folder != nil {
		if folder.FolderID != parentFolderID {
			return nil, fmt.Errorf("existing record with different parent found")
		}
		folder.Folder = f
		folder.Category = c
		return folder, nil
	}

	var _, filename = filepath.Split(path)
	var newFolder = Folder{
		Folder:     f,
		FolderID:   parentFolderID,
		Category:   c,
		CategoryID: c.ID,
		Depth:      strings.Count(path, string(os.PathSeparator)),
		PublicPath: path,
		Name:       filename,
	}
	op.Folders.Save(&newFolder)
	return &newFolder, op.Operation.Err()
}

// FindRealFolderByPath looks for a folder with the given path under the given category
func (op *Operation) FindRealFolderByPath(f *Folder, path string) (*RealFolder, error) {
	var folder = &RealFolder{}
	var ok = op.RealFolders.Select().Where("folder_id = ? AND full_path = ?", f.ID, path).First(folder)
	if !ok {
		folder = nil
	}
	return folder, op.Operation.Err()
}

// FindOrCreateRealFolder centralizes the creation and DB-save operation for real_folders
func (op *Operation) FindOrCreateRealFolder(f *Folder, path string) (*RealFolder, error) {
	var fid = 0
	if f != nil {
		fid = f.ID
	}
	var folder, err = op.FindRealFolderByPath(f, path)
	if err != nil {
		return nil, err
	}
	if folder != nil {
		if folder.FolderID != fid {
			return nil, fmt.Errorf("existing record with different folder found")
		}
		folder.Folder = f
		return folder, nil
	}

	var newFolder = RealFolder{
		Folder:   f,
		FolderID: fid,
		FullPath: path,
	}
	op.RealFolders.Save(&newFolder)
	return &newFolder, op.Operation.Err()
}

// GetFolders returns all folders with the given category and parent folder.  A
// parent folder of nil can be used to pull all top-level folders.
func (op *Operation) GetFolders(category *Category, folder *Folder) ([]*Folder, error) {
	var sel = op.FolderSelect(category, folder)
	var folders []*Folder
	var _, err = sel.AllObjects(&folders)
	return folders, err
}

// GetFiles returns all files with the given category and parent folder.  A
// parent folder of nil can be used to pull all top-level files.
func (op *Operation) GetFiles(category *Category, folder *Folder, limit uint64) ([]*File, uint64, error) {
	var sel = op.FileSelect(category, folder).Limit(limit)
	var files []*File
	var count, err = sel.AllObjects(&files)
	return files, count, err
}

// SearchFiles finds all files which are *descendents* of the given
// category/folder and match the term
//
// Note that folder data is *not* filled in on the returns files.  Pulling
// folders from the database is unnecessary since all folder lookups are via
// path, so this reduces the amount of information we pull from the database
// and simplifies the code quite a bit.
func (op *Operation) SearchFiles(category *Category, folder *Folder, term string, limit uint64) ([]*File, uint64, error) {
	var sel = op.FileSelect(category, folder).TreeMode(true).Search("public_path LIKE ?", term).Limit(limit)
	var files []*File
	var count, err = sel.AllObjects(&files)
	return files, count, err
}

// SearchFolders finds all folders which are *descendents* of the given
// category/folder and match the term
//
// Note that parent folder data is *not* filled in on the returns files.
// Pulling folders from the database is unnecessary since all folder lookups
// are via path, so this reduces the amount of information we pull from the
// database and simplifies the code quite a bit.
func (op *Operation) SearchFolders(category *Category, folder *Folder, term string, limit uint64) ([]*Folder, uint64, error) {
	var sel = op.FolderSelect(category, folder).TreeMode(true).Search("name LIKE ?", term).Limit(limit)
	var folders []*Folder
	var count, err = sel.AllObjects(&folders)
	return folders, count, err
}

// FindFileByID returns the file found by the given ID, or nil if none if
// found.  Any database errors are passed back to the caller.
func (op *Operation) FindFileByID(id uint64) (*File, error) {
	var file = &File{}
	var ok = op.Files.Select().Where("id = ?", id).First(file)
	if !ok {
		file = nil
	}
	return file, op.Operation.Err()
}

// PopulateCategories fills in the category data for all passed-in files and folders
func (op *Operation) PopulateCategories(files []*File, folders []*Folder) error {
	var categoryLookup = make(map[int]*Category)
	var categoryList, err = op.AllCategories()
	if err != nil {
		return err
	}
	for _, c := range categoryList {
		categoryLookup[c.ID] = c
	}
	for _, f := range files {
		f.Category = categoryLookup[f.CategoryID]
	}
	for _, f := range folders {
		f.Category = categoryLookup[f.CategoryID]
	}
	return nil
}

func (op *Operation) appendFiles(files []*File, ids []uint64) []*File {
	var where = "id IN (" + strings.Repeat("?, ", len(ids)-1) + "?)"
	var args []interface{}
	for _, id := range ids {
		args = append(args, id)
	}
	var tempFiles []*File
	op.Files.Select().Where(where, args...).AllObjects(&tempFiles)
	return append(files, tempFiles...)
}

// GetFilesByIDs returns a list of File instances for the given ids
func (op *Operation) GetFilesByIDs(ids []uint64) ([]*File, error) {
	var files []*File

	// split ids into chunks that can work in an IN query - I can't find any
	// details on what a real limit might be, so we just split at 1000 and hope
	// for the best
	for len(ids) > 1000 {
		files = op.appendFiles(files, ids[:1000])
		ids = ids[1000:]
	}
	if len(ids) > 0 {
		files = op.appendFiles(files, ids)
	}

	// We have to sort after the fact since we don't know how many passes it took
	// to get the data
	sort.Slice(files, func(i, j int) bool {
		if files[i].Depth != files[j].Depth {
			return files[i].Depth < files[j].Depth
		}
		return strings.ToLower(files[i].PublicPath) < strings.ToLower(files[j].PublicPath)
	})

	op.PopulateCategories(files, nil)
	return files, op.Operation.Err()
}

// QueueArchiveJob creates a new archive job in the database for async processing
func (op *Operation) QueueArchiveJob(addrs []*mail.Address, files []*File) error {
	if len(files) == 0 {
		return fmt.Errorf("no files to archive")
	}

	if len(addrs) == 0 {
		return fmt.Errorf("no notification addresses for archive job")
	}

	var filePaths []string
	for _, f := range files {
		filePaths = append(filePaths, f.FullPath)
	}

	var emails []string
	for _, addr := range addrs {
		emails = append(emails, addr.String())
	}

	op.ArchiveJobs.Save(&ArchiveJob{
		CreatedAt:          time.Now(),
		NotificationEmails: strings.Join(emails, ","),
		Files:              strings.Join(filePaths, "\x1E"),
	})
	return op.Operation.Err()
}

// ProcessArchiveJob pulls the longest-waiting archive job and runs the
// callback with it.  If the callback returns success, the archive job is
// removed from the database.  If no job is found, the callback isn't run.
func (op *Operation) ProcessArchiveJob(cb func(*ArchiveJob) bool) error {
	var j = &ArchiveJob{}
	var sel = op.ArchiveJobs.Select().Where("next_attempt_at < ? AND processed = ?", time.Now(), false)
	var ok = sel.Order("created_at ASC").Limit(1).First(j)
	if op.Operation.Err() != nil {
		return op.Operation.Err()
	}
	if !ok {
		return nil
	}

	if cb(j) {
		j.Processed = true
	} else {
		j.NextAttemptAt = time.Now().Add(time.Hour)
	}

	op.ArchiveJobs.Save(j)
	return op.Operation.Err()
}

// GetRealFolders returns real folders that can get to the given collapsed /
// public folder
func (op *Operation) GetRealFolders(f *Folder) ([]*RealFolder, error) {
	var folders []*RealFolder
	op.RealFolders.Select().Where("folder_id = ?", f.ID).AllObjects(&folders)
	return folders, op.Operation.Err()
}
