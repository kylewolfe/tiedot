/* Document management and index maintenance. */
package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"

	"github.com/HouzuoGuo/tiedot/tdlog"
)

// Resolve the attribute(s) in the document structure along the given path.
func GetIn(doc interface{}, path []string) (ret []interface{}) {
	docMap, ok := doc.(map[string]interface{})
	if !ok {
		return
	}
	var thing interface{} = docMap
	// Get into each path segment
	for i, seg := range path {
		if aMap, ok := thing.(map[string]interface{}); ok {
			thing = aMap[seg]
		} else if anArray, ok := thing.([]interface{}); ok {
			for _, element := range anArray {
				ret = append(ret, GetIn(element, path[i:])...)
			}
			return ret
		} else {
			return nil
		}
	}
	switch thing.(type) {
	case []interface{}:
		return append(ret, thing.([]interface{})...)
	default:
		return append(ret, thing)
	}
}

// Hash a string using sdbm algorithm.
func StrHash(str string) int {
	var hash int
	for _, c := range str {
		hash = int(c) + (hash << 6) + (hash << 16) - hash
	}
	if hash < 0 {
		return -hash
	}
	return hash
}

// Put a document on all user-created indexes.
func (col *Col) indexDoc(id int, doc map[string]interface{}) {
	for idxName, idxPath := range col.indexPaths {
		for _, idxVal := range GetIn(doc, idxPath) {
			if idxVal != nil {
				hashKey := StrHash(fmt.Sprint(idxVal))
				partNum := hashKey % col.db.numParts
				ht := col.hts[partNum][idxName]
				ht.Lock.Lock()
				ht.Put(hashKey, id)
				ht.Lock.Unlock()
			}
		}
	}
}

// Remove a document from all user-created indexes.
func (col *Col) unindexDoc(id int, doc map[string]interface{}) {
	for idxName, idxPath := range col.indexPaths {
		for _, idxVal := range GetIn(doc, idxPath) {
			if idxVal != nil {
				hashKey := StrHash(fmt.Sprint(idxVal))
				partNum := hashKey % col.db.numParts
				ht := col.hts[partNum][idxName]
				ht.Lock.Lock()
				ht.Remove(hashKey, id)
				ht.Lock.Unlock()
			}
		}
	}
}

// Insert a document with the specified ID into the collection (incl. index). Does not place partition/schema lock.
func (col *Col) InsertRecovery(id int, doc map[string]interface{}) (err error) {
	docJS, err := json.Marshal(doc)
	if err != nil {
		return
	}
	partNum := id % col.db.numParts
	part := col.parts[partNum]
	// Put document data into collection
	if _, err = part.Insert(id, []byte(docJS)); err != nil {
		return
	}
	// Index the document
	col.indexDoc(id, doc)
	return
}

// Insert a document into the collection.
func (col *Col) Insert(doc interface{}) (id int, err error) {
	var d Document               // our base document format
	var m map[string]interface{} // format needed for indexing

	id = rand.Int()

	// cast given type accordingly
	switch dt := doc.(type) {
	case Document:
		d = dt
		if m, err = d.Map(); err != nil {
			return 0, err
		}
	case []byte:
		d = Document(dt)
		if m, err = d.Map(); err != nil {
			return 0, err
		}
	case map[string]interface{}:
		if d, err = docFromMap(dt); err != nil {
			return 0, err
		}
		m = dt
	default:
		// determine if a pointer to struct
		t := reflect.TypeOf(doc)
		v := reflect.ValueOf(doc)
		if t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Struct {
			return 0, errors.New("Unknown data type given, must be a strcut pointer, Document, []byte, or map[string]interface{}")
		}

		// convert struct to doc
		var b []byte
		if b, err = json.Marshal(doc); err != nil {
			return 0, err
		}

		// update tiedot id if in struct
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.PkgPath == "" && strings.Contains(f.Tag.Get("tiedot"), "id") == false { // TODO: this is hacky... fix it
				if v.CanSet() {
					if v.Kind() == reflect.Int {
						if !v.OverflowInt(int64(id)) { // TODO: right way to handle for both 32 and 64 bit?
							v.SetInt(int64(id))
						}
					}
				}
			}
		}

		d = Document(b)
	}

	partNum := id % col.db.numParts
	col.db.schemaLock.RLock()
	part := col.parts[partNum]

	// Put document data into collection
	part.Lock.Lock()
	if _, err = part.Insert(id, []byte(d)); err != nil {
		part.Lock.Unlock()
		col.db.schemaLock.RUnlock()
		return
	}

	// If another thread is updating the document in the meanwhile, let it take over index maintenance
	if err = part.LockUpdate(id); err != nil {
		part.Lock.Unlock()
		col.db.schemaLock.RUnlock()
		return id, nil
	}
	part.Lock.Unlock()

	// Index the document
	col.indexDoc(id, m)
	part.Lock.Lock()
	part.UnlockUpdate(id)
	part.Lock.Unlock()
	col.db.schemaLock.RUnlock()

	return
}

func (col *Col) read(id int, placeSchemaLock bool) (doc map[string]interface{}, err error) {
	if placeSchemaLock {
		col.db.schemaLock.RLock()
	}
	part := col.parts[id%col.db.numParts]
	part.Lock.RLock()
	docB, err := part.Read(id)
	part.Lock.RUnlock()
	if err != nil {
		if placeSchemaLock {
			col.db.schemaLock.RUnlock()
		}
		return
	}
	err = json.Unmarshal(docB, &doc)
	if placeSchemaLock {
		col.db.schemaLock.RUnlock()
	}
	return
}

// Find and retrieve a document by ID.
func (col *Col) Read(id int) (doc map[string]interface{}, err error) {
	return col.read(id, true)
}

// Update a document.
func (col *Col) Update(id int, doc map[string]interface{}) error {
	if doc == nil {
		return fmt.Errorf("Updating %d: input doc may not be nil", id)
	}
	docJS, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	col.db.schemaLock.RLock()
	part := col.parts[id%col.db.numParts]
	part.Lock.Lock()
	// Place lock, read back original document and update
	if err := part.LockUpdate(id); err != nil {
		part.Lock.Unlock()
		col.db.schemaLock.RUnlock()
		return err
	}
	originalB, err := part.Read(id)
	if err != nil {
		part.UnlockUpdate(id)
		part.Lock.Unlock()
		col.db.schemaLock.RUnlock()
		return err
	}
	var original map[string]interface{}
	if err = json.Unmarshal(originalB, &original); err != nil {
		tdlog.Noticef("Will not attempt to unindex document %d during update", id)
	}
	if err = part.Update(id, []byte(docJS)); err != nil {
		part.UnlockUpdate(id)
		part.Lock.Unlock()
		col.db.schemaLock.RUnlock()
		return err
	}
	// Done with the collection data, next is to maintain indexed values
	part.Lock.Unlock()
	if original != nil {
		col.unindexDoc(id, original)
	}
	col.indexDoc(id, doc)
	// Done with the document
	part.Lock.Lock()
	part.UnlockUpdate(id)
	part.Lock.Unlock()
	col.db.schemaLock.RUnlock()
	return nil
}

// Delete a document.
func (col *Col) Delete(id int) error {
	col.db.schemaLock.RLock()
	part := col.parts[id%col.db.numParts]
	part.Lock.Lock()
	// Place lock, read back original document and delete document
	if err := part.LockUpdate(id); err != nil {
		part.Lock.Unlock()
		col.db.schemaLock.RUnlock()
		return err
	}
	originalB, err := part.Read(id)
	if err != nil {
		part.UnlockUpdate(id)
		part.Lock.Unlock()
		col.db.schemaLock.RUnlock()
		return err
	}
	var original map[string]interface{}
	if err = json.Unmarshal(originalB, &original); err != nil {
		tdlog.Noticef("Will not attempt to unindex document %d during delete", id)
	}
	if err = part.Delete(id); err != nil {
		part.UnlockUpdate(id)
		part.Lock.Unlock()
		col.db.schemaLock.RUnlock()
		return err
	}
	// Done with the collection data, next is to remove indexed values
	part.Lock.Unlock()
	if original != nil {
		col.unindexDoc(id, original)
	}
	part.Lock.Lock()
	part.UnlockUpdate(id)
	part.Lock.Unlock()
	col.db.schemaLock.RUnlock()
	return nil
}
