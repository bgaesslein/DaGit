package main

import (
	"bytes"
	"compress/zlib"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/schollz/progressbar/v3"
)

// Given a byte find the first byte in a data slice that equals the match_byte, returning the index.
// If no match is found, returns -1
func findFirstMatch(match_byte byte, start_index int, data *[]byte) int {
	for i, this_byte := range (*data)[start_index:] {
		if this_byte == match_byte {
			return start_index + i
		}
	}
	return -1
}

const (
	SPACE    byte   = 32
	NUL      byte   = 0
	GIT_DIR  string = ".git"
	OBJS_DIR string = "/.git/objects"
	HEAD_LOC string = "/.git/HEAD"
)

type Edge struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

type Object struct {
	Type_    string `json:"type"`
	Size     string `json:"size"`
	Location string `json:"location"`
	Name     string `json:"name"`
	Content  []byte `json:"content"`
}

type TreeEntry struct {
	Mode string `json:"mode"`
	Name string `json:"name"`
	Hash string `json:"hash"`
}

type Commit struct {
	Tree    string   `json:"tree"`
	Parents []string `json:"parents"`
}

type Repo struct {
	location string
	objects  map[string]*Object
}

//func (gd *GraphData) MarshalJSON() ([]byte, error) {
//	return []byte(`{"data":"charlie"}`), nil
//}

func getType(data *[]byte) (string, int) {
	first_space_index := findFirstMatch(SPACE, 0, data)
	type_ := string((*data)[0:first_space_index])
	return strings.TrimSpace(type_), first_space_index
}

// second return value is the start of the object's content
func getSize(first_space_index int, data *[]byte) (string, int) {
	first_nul_index := findFirstMatch(NUL, first_space_index+1, data)
	obj_size := string((*data)[first_space_index:first_nul_index])
	return strings.TrimSpace(obj_size), first_nul_index + 1
}

func newObject(object_path string) *Object {
	zlib_bytes, err := os.ReadFile(object_path)
	if err != nil {
		log.Fatal(err)
	}
	// zlib expects an io.Reader object
	reader, err := zlib.NewReader(bytes.NewReader(zlib_bytes))
	if err != nil {
		log.Fatal(err)
	}
	bytes, err := io.ReadAll(reader)
	if err != nil {
		log.Fatal(err)
	}
	data_ptr := &bytes
	type_, first_space_index := getType(data_ptr)
	size, content_start_index := getSize(first_space_index, data_ptr)
	object_dir := filepath.Base(filepath.Dir(object_path))
	return &Object{type_, size, object_path, object_dir + filepath.Base(object_path), bytes[content_start_index:]}
}

func (obj *Object) toJson() []byte {
	switch obj.Type_ {
	case "tree":
		json_tree, err := json.Marshal(map[string][]TreeEntry{"entries": *parseTree(obj)})
		if err != nil {
			log.Fatal(err)
		}
		return json_tree
	case "commit":
		json_commit, err := json.Marshal(parseCommit(obj))
		if err != nil {
			log.Fatal(err)
		}
		return json_commit
	case "blob":
		json_blob, err := json.Marshal(obj)
		if err != nil {
			log.Fatal(err)
		}
		return json_blob
	default:
		return make([]byte, 0)
	}
}

func getObjectName(object_path string) string {
	object_dir := filepath.Base(filepath.Dir(object_path))
	name := object_dir + filepath.Base(object_path)
	return name
}

func getObjects(objects_dir string) map[string]*Object {
	objects := make(map[string]*Object)
	filepath.WalkDir(objects_dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		is_hex, err := regexp.MatchString("^[a-fA-F0-9]+$", filepath.Base(path))
		if err != nil {
			log.Fatal(err)
		}
		if !d.IsDir() && is_hex {
			obj := newObject(path)
			objects[obj.Name] = obj
		}
		return nil
	})
	return objects
}

func newRepo(location string) *Repo {
	objects := getObjects(location + OBJS_DIR)
	return &Repo{location, objects}
}

func (r *Repo) getObject(name string) *Object {
	return r.objects[name]
}

func (r *Repo) toJson() []byte {
	edges := []Edge{}
	nodes := []map[string]any{}
	for _, obj := range r.objects {
		var objMap map[string]json.RawMessage
		err := json.Unmarshal(obj.toJson(), &objMap)
		if err != nil {
			log.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"name": obj.Name, "type": obj.Type_, "object": objMap})
		switch obj.Type_ {
		case "commit":
			commit := parseCommit(obj)
			// commit edges to parents
			for _, p := range commit.Parents {
				edges = append(edges, Edge{Src: obj.Name, Dest: p})
			}
			// commit edge to tree
			edges = append(edges, Edge{Src: obj.Name, Dest: commit.Tree})
		case "tree":
			entries := *parseTree(obj)
			// tree to blob edges
			for _, entry := range entries {
				edges = append(edges, Edge{Src: obj.Name, Dest: entry.Hash})
			}
		}
	}
	repo_json, err := json.Marshal(map[string]any{"nodes": nodes, "edges": edges})
	if err != nil {
		log.Fatal(err)
	}
	return repo_json
}

func exec(db *sql.DB, query string) sql.Result {
	result, err := db.Exec(query)
	if err != nil {
		log.Fatal(err)
	}
	return result
}

func (r *Repo) toSQLite(path string) {
	os.Remove(path)

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	exec(db, `create table objects (name text primary key, type text, object jsonb);`)
	exec(db, `create table edges (src text, dest text);`)
	objs_stmt, err := db.Prepare("insert into objects(name, type, object) values(?, ?, ?)")
	if err != nil {
		log.Fatal(err)
	}
	edges_stmt, err := db.Prepare("insert into edges(src, dest) values(?, ?)")
	if err != nil {
		log.Fatal(err)
	}
	defer objs_stmt.Close()
	defer edges_stmt.Close()

	fmt.Println("[info] generating Git SQLite database...")
	bar := progressbar.Default(int64(len(r.objects)))
	for name, obj := range r.objects {
		_, err = objs_stmt.Exec(name, obj.Type_, obj.toJson())
		if err != nil {
			log.Fatal(err)
		}
		switch obj.Type_ {
		case "commit":
			commit := parseCommit(obj)
			// commit edges to parents
			for _, p := range commit.Parents {
				_, err = edges_stmt.Exec(obj.Name, p)
				if err != nil {
					log.Fatal(err)
				}
			}
			// commit edge to tree
			_, err = edges_stmt.Exec(obj.Name, commit.Tree)
			if err != nil {
				log.Fatal(err)
			}
		case "tree":
			entries := *parseTree(obj)
			// tree to blob edges
			for _, entry := range entries {
				_, err = edges_stmt.Exec(obj.Name, entry.Hash)
				if err != nil {
					log.Fatal(err)
				}
			}
		}
		bar.Add(1)
	}
}

func (r *Repo) refresh() {
	objects := getObjects(r.location)
	r.objects = objects
}

func (r *Repo) head() string {
	bytes, err := os.ReadFile(r.location + HEAD_LOC)
	if err != nil {
		log.Fatal(err)
	}
	return strings.TrimSpace(strings.Split(string(bytes), ":")[1])
}

func (r *Repo) branch() string {
	return filepath.Base(r.head())
}

func (r *Repo) currentCommit() Commit {
	bytes, err := os.ReadFile(r.location + fmt.Sprintf("/%s/", GIT_DIR) + r.head())
	if err != nil {
		log.Fatal(err)
	}
	return *parseCommit(r.getObject(strings.TrimSpace(string(bytes))))
}

func parseTree(obj *Object) *[]TreeEntry {
	var entries []TreeEntry
	content_len := len(obj.Content)
	entry_item, start, stop := 1, 0, 6 // TODO: don't use magic numbers. Define constants.
	mode, name, hash := "", "", ""
	for stop <= content_len {
		switch entry_item {
		// get the mode
		case 1:
			mode = strings.TrimSpace(string(obj.Content[start:stop]))
			entry_item += 1
			start = stop
		// get the name (file or dir)
		case 2:
			i := start
			for obj.Content[i] != NUL && i < content_len-1 {
				i += 1
			}
			name = strings.TrimSpace(string(obj.Content[start:i]))
			entry_item += 1
			start = i + 1
			stop = start + 20 // TODO: don't use magic numbers. Define constants.
		// get the hash (object name)
		case 3:
			hash = strings.TrimSpace(hex.EncodeToString(obj.Content[start:stop]))
			entry_item = 1
			start = stop
			stop = start + 6 // TODO: don't use magic numbers. Define constants.
			entries = append(entries, TreeEntry{mode, name, hash})
		}
	}
	return &entries
}

func parseCommit(obj *Object) *Commit {
	tree_hash := string(obj.Content[5:45])                           // TODO: don't use magic numbers. Define constants.
	rest_of_content := strings.Split(string(obj.Content[46:]), "\n") // TODO: don't use magic numbers. Define constants.
	var parents []string
	for _, line := range rest_of_content {
		if line[:6] == "parent" {
			parents = append(parents, line[7:47]) // TODO: don't use magic numbers. Define constants.
		} else {
			break
		}
	}
	return &Commit{tree_hash, parents}
}