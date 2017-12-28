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
	mtProjects    *magicsql.MagicTable
	mtInventories *magicsql.MagicTable
	mtZipJobs     *magicsql.MagicTable
}

// Operation wraps a magicsql Operation with preloaded OperationTable
// definitions for easy querying
type Operation struct {
	Operation   *magicsql.Operation
	Files       *magicsql.OperationTable
	Folders     *magicsql.OperationTable
	Inventories *magicsql.OperationTable
	Projects    *magicsql.OperationTable
	ZipJobs     *magicsql.OperationTable
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
		mtProjects:    magicsql.Table("projects", &Project{}),
		mtInventories: magicsql.Table("inventories", &Inventory{}),
		mtZipJobs:     magicsql.Table("zip_jobs", &ZipJob{}),
	}
}

// Operation returns a pre-set Operation for quick tasks that don't warrant a transaction
func (db *Database) Operation() *Operation {
	var magicOp = db.dbh.Operation()
	return &Operation{
		Operation:   magicOp,
		Files:       magicOp.OperationTable(db.mtFiles),
		Folders:     magicOp.OperationTable(db.mtFolders),
		Inventories: magicOp.OperationTable(db.mtInventories),
		Projects:    magicOp.OperationTable(db.mtProjects),
		ZipJobs:     magicOp.OperationTable(db.mtZipJobs),
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

// AllProjects returns all projects which have been seen
func (op *Operation) AllProjects() ([]*Project, error) {
	var projects []*Project
	op.Projects.Select().Order("LOWER(name)").AllObjects(&projects)
	return projects, op.Operation.Err()
}

// FindProjectByName returns a project if one exists with the given name, and
// the database error if any occurred
func (op *Operation) FindProjectByName(name string) (*Project, error) {
	var project = &Project{}
	var ok = op.Projects.Select().Where("name = ?", name).First(project)
	if !ok {
		project = nil
	}
	return project, op.Operation.Err()
}

// FindOrCreateProject stores (or finds) the project by the given name and
// returns it.  If there are any database errors, they're returned and Project
// will be undefined.
func (op *Operation) FindOrCreateProject(name string) (*Project, error) {
	var project, err = op.FindProjectByName(name)
	if project == nil && err == nil {
		project = &Project{Name: name}
		op.Projects.Save(project)
	}
	return project, op.Operation.Err()
}

// FindFolderByPath looks for a folder with the given path under the given project
func (op *Operation) FindFolderByPath(p *Project, path string) (*Folder, error) {
	var folder = &Folder{}
	var ok = op.Folders.Select().Where("project_id = ? AND public_path = ?", p.ID, path).First(folder)
	if !ok {
		folder = nil
	}
	return folder, op.Operation.Err()
}

// FindOrCreateFolder centralizes the creation and DB-save operation for folders
func (op *Operation) FindOrCreateFolder(p *Project, f *Folder, path string) (*Folder, error) {
	var parentFolderID = 0
	if f != nil {
		parentFolderID = f.ID
	}
	var folder, err = op.FindFolderByPath(p, path)
	if err != nil {
		return nil, err
	}
	if folder != nil {
		if folder.FolderID != parentFolderID {
			return nil, fmt.Errorf("existing record with different parent found")
		}
		folder.Folder = f
		folder.Project = p
		return folder, nil
	}

	var _, filename = filepath.Split(path)
	var newFolder = Folder{
		Folder:     f,
		FolderID:   parentFolderID,
		Project:    p,
		ProjectID:  p.ID,
		Depth:      strings.Count(path, string(os.PathSeparator)),
		PublicPath: path,
		Name:       filename,
	}
	op.Folders.Save(&newFolder)
	return &newFolder, op.Operation.Err()
}

// GetFolders returns all folders with the given project and parent folder.  A
// parent folder of nil can be used to pull all top-level folders.
func (op *Operation) GetFolders(project *Project, folder *Folder) ([]*Folder, error) {
	var sel = op.FolderSelect(project, folder)
	var folders []*Folder
	var _, err = sel.AllObjects(&folders)
	return folders, err
}

// GetFiles returns all files with the given project and parent folder.  A
// parent folder of nil can be used to pull all top-level files.
func (op *Operation) GetFiles(project *Project, folder *Folder, limit uint64) ([]*File, uint64, error) {
	var sel = op.FileSelect(project, folder).Limit(limit)
	var files []*File
	var count, err = sel.AllObjects(&files)
	return files, count, err
}

// SearchFiles finds all files which are *descendents* of the given
// project/folder and match the term
//
// Note that folder data is *not* filled in on the returns files.  Pulling
// folders from the database is unnecessary since all folder lookups are via
// path, so this reduces the amount of information we pull from the database
// and simplifies the code quite a bit.
func (op *Operation) SearchFiles(project *Project, folder *Folder, term string, limit uint64) ([]*File, uint64, error) {
	var sel = op.FileSelect(project, folder).TreeMode(true).Search("public_path LIKE ?", term).Limit(limit)
	var files []*File
	var count, err = sel.AllObjects(&files)
	return files, count, err
}

// SearchFolders finds all folders which are *descendents* of the given
// project/folder and match the term
//
// Note that parent folder data is *not* filled in on the returns files.
// Pulling folders from the database is unnecessary since all folder lookups
// are via path, so this reduces the amount of information we pull from the
// database and simplifies the code quite a bit.
func (op *Operation) SearchFolders(project *Project, folder *Folder, term string, limit uint64) ([]*Folder, uint64, error) {
	var sel = op.FolderSelect(project, folder).TreeMode(true).Search("name LIKE ?", term).Limit(limit)
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

// PopulateProjects fills in the project data for all passed-in files and folders
func (op *Operation) PopulateProjects(files []*File, folders []*Folder) error {
	var projectLookup = make(map[int]*Project)
	var projectList, err = op.AllProjects()
	if err != nil {
		return err
	}
	for _, p := range projectList {
		projectLookup[p.ID] = p
	}
	for _, f := range files {
		f.Project = projectLookup[f.ProjectID]
	}
	for _, f := range folders {
		f.Project = projectLookup[f.ProjectID]
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

	op.PopulateProjects(files, nil)
	return files, op.Operation.Err()
}

// QueueZipJob creates a new zip job in the database for async processing
func (op *Operation) QueueZipJob(addrs []*mail.Address, files []*File) error {
	if len(files) == 0 {
		return fmt.Errorf("no files to zip")
	}

	if len(addrs) == 0 {
		return fmt.Errorf("no notification addresses for zip job")
	}

	var filePaths []string
	for _, f := range files {
		filePaths = append(filePaths, f.FullPath)
	}

	var emails []string
	for _, addr := range addrs {
		emails = append(emails, addr.String())
	}

	op.ZipJobs.Save(&ZipJob{
		CreatedAt:          time.Now(),
		NotificationEmails: strings.Join(emails, ","),
		Files:              strings.Join(filePaths, "\x1E"),
	})
	return op.Operation.Err()
}

// ProcessZipJob pulls the longest-waiting zip job and runs the callback with
// it.  If the callback returns success, the zip job is removed from the
// database.  If no zip job is found, the callback isn't run.
func (op *Operation) ProcessZipJob(cb func(*ZipJob) bool) error {
	var zj = &ZipJob{}
	var ok = op.ZipJobs.Select().Order("created_at ASC").Limit(1).First(zj)
	if !ok {
		return nil
	}
	if op.Operation.Err() != nil {
		return op.Operation.Err()
	}

	if cb(zj) {
		op.Operation.Exec("DELETE FROM zip_jobs WHERE id = ?", zj.ID)
		return op.Operation.Err()
	}

	return nil
}
