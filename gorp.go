// Copyright 2012 James Cooper. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// Package gorp provides a simple way to marshal Go structs to and from
// SQL databases.  It uses the database/sql package, and should work with any
// compliant database/sql driver.
//
// Source code and project home:
// https://github.com/coopernurse/gorp
//
package gorp

import (
	"bytes"
	"database/sql"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

var zeroVal reflect.Value
var versFieldConst = "[gorp_ver_field]"

// OptimisticLockError is returned by Update() or Delete() if the
// struct being modified has a Version field and the value is not equal to
// the current value in the database
type OptimisticLockError struct {
	// Table name where the lock error occurred
	TableName string

	// Primary key values of the row being updated/deleted
	Keys []interface{}

	// true if a row was found with those keys, indicating the
	// LocalVersion is stale.  false if no value was found with those
	// keys, suggesting the row has been deleted since loaded, or
	// was never inserted to begin with
	RowExists bool

	// Version value on the struct passed to Update/Delete. This value is
	// out of sync with the database.
	LocalVersion int64
}

// Error returns a description of the cause of the lock error
func (e OptimisticLockError) Error() string {
	if e.RowExists {
		return fmt.Sprintf("gorp: OptimisticLockError table=%s keys=%v out of date version=%d", e.TableName, e.Keys, e.LocalVersion)
	}

	return fmt.Sprintf("gorp: OptimisticLockError no row found for table=%s keys=%v", e.TableName, e.Keys)
}

// The TypeConverter interface provides a way to map a value of one
// type to another type when persisting to, or loading from, a database.
//
// Example use cases: Implement type converter to convert bool types to "y"/"n" strings,
// or serialize a struct member as a JSON blob.
type TypeConverter interface {
	// ToDb converts val to another type. Called before INSERT/UPDATE operations
	ToDb(val interface{}) (interface{}, error)

	// FromDb returns a CustomScanner appropriate for this type. This will be used
	// to hold values returned from SELECT queries.
	//
	// In particular the CustomScanner returned should implement a Binder
	// function appropriate for the Go type you wish to convert the db value to
	//
	// If bool==false, then no custom scanner will be used for this field.
	FromDb(target interface{}) (CustomScanner, bool)
}

// CustomScanner binds a database column value to a Go type
type CustomScanner struct {
	// After a row is scanned, Holder will contain the value from the database column.
	// Initialize the CustomScanner with the concrete Go type you wish the database
	// driver to scan the raw column into.
	Holder interface{}
	// Target typically holds a pointer to the target struct field to bind the Holder
	// value to.
	Target interface{}
	// Binder is a custom function that converts the holder value to the target type
	// and sets target accordingly.  This function should return error if a problem
	// occurs converting the holder to the target.
	Binder func(holder interface{}, target interface{}) error
}

// Bind is called automatically by gorp after Scan()
func (me CustomScanner) Bind() error {
	return me.Binder(me.Holder, me.Target)
}

type bindPlan struct {
	query             string
	argFields         []string
	keyFields         []string
	versField         string
	autoIncrIdx       int
	autoIncrFieldName string
}

func (plan bindPlan) createBindInstance(elem reflect.Value, conv TypeConverter) (bindInstance, error) {
	bi := bindInstance{query: plan.query, autoIncrIdx: plan.autoIncrIdx, autoIncrFieldName: plan.autoIncrFieldName, versField: plan.versField}
	if plan.versField != "" {
		bi.existingVersion = elem.FieldByName(plan.versField).Int()
	}

	var err error

	for i := 0; i < len(plan.argFields); i++ {
		k := plan.argFields[i]
		if k == versFieldConst {
			newVer := bi.existingVersion + 1
			bi.args = append(bi.args, newVer)
			if bi.existingVersion == 0 {
				elem.FieldByName(plan.versField).SetInt(int64(newVer))
			}
		} else {
			val := elem.FieldByName(k).Interface()
			if conv != nil {
				val, err = conv.ToDb(val)
				if err != nil {
					return bindInstance{}, err
				}
			}
			bi.args = append(bi.args, val)
		}
	}

	for i := 0; i < len(plan.keyFields); i++ {
		k := plan.keyFields[i]
		val := elem.FieldByName(k).Interface()
		if conv != nil {
			val, err = conv.ToDb(val)
			if err != nil {
				return bindInstance{}, err
			}
		}
		bi.keys = append(bi.keys, val)
	}

	return bi, nil
}

type bindInstance struct {
	query             string
	args              []interface{}
	keys              []interface{}
	existingVersion   int64
	versField         string
	autoIncrIdx       int
	autoIncrFieldName string
}

func (t *TableMap) bindInsert(elem reflect.Value) (bindInstance, error) {
	plan := t.insertPlan
	if plan.query == "" {
		plan.autoIncrIdx = -1

		s := bytes.Buffer{}
		s2 := bytes.Buffer{}
		s.WriteString(fmt.Sprintf("insert into %s (", t.dbmap.Dialect.QuotedTableForQuery(t.SchemaName, t.TableName)))

		x := 0
		first := true
		for y := range t.columns {
			col := t.columns[y]

			if !col.Transient {
				if !first {
					s.WriteString(",")
					s2.WriteString(",")
				}
				s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))

				if col.isAutoIncr {
					s2.WriteString(t.dbmap.Dialect.AutoIncrBindValue())
					plan.autoIncrIdx = y
					plan.autoIncrFieldName = col.fieldName
				} else {
					s2.WriteString(t.dbmap.Dialect.BindVar(x))
					if col == t.version {
						plan.versField = col.fieldName
						plan.argFields = append(plan.argFields, versFieldConst)
					} else {
						plan.argFields = append(plan.argFields, col.fieldName)
					}

					x++
				}

				first = false
			}
		}
		s.WriteString(") values (")
		s.WriteString(s2.String())
		s.WriteString(")")
		if plan.autoIncrIdx > -1 {
			s.WriteString(t.dbmap.Dialect.AutoIncrInsertSuffix(t.columns[plan.autoIncrIdx]))
		}
		s.WriteString(";")

		plan.query = s.String()
		t.insertPlan = plan
	}

	return plan.createBindInstance(elem, t.dbmap.TypeConverter)
}

func (t *TableMap) bindUpdate(elem reflect.Value) (bindInstance, error) {
	plan := t.updatePlan
	if plan.query == "" {

		s := bytes.Buffer{}
		s.WriteString(fmt.Sprintf("update %s set ", t.dbmap.Dialect.QuotedTableForQuery(t.SchemaName, t.TableName)))
		x := 0

		for y := range t.columns {
			col := t.columns[y]
			if !col.isPK && !col.Transient {
				if x > 0 {
					s.WriteString(", ")
				}
				s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
				s.WriteString("=")
				s.WriteString(t.dbmap.Dialect.BindVar(x))

				if col == t.version {
					plan.versField = col.fieldName
					plan.argFields = append(plan.argFields, versFieldConst)
				} else {
					plan.argFields = append(plan.argFields, col.fieldName)
				}
				x++
			}
		}

		s.WriteString(" where ")
		for y := range t.keys {
			col := t.keys[y]
			if y > 0 {
				s.WriteString(" and ")
			}
			s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))

			plan.argFields = append(plan.argFields, col.fieldName)
			plan.keyFields = append(plan.keyFields, col.fieldName)
			x++
		}
		if plan.versField != "" {
			s.WriteString(" and ")
			s.WriteString(t.dbmap.Dialect.QuoteField(t.version.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))
			plan.argFields = append(plan.argFields, plan.versField)
		}
		s.WriteString(";")

		plan.query = s.String()
		t.updatePlan = plan
	}

	return plan.createBindInstance(elem, t.dbmap.TypeConverter)
}

func (t *TableMap) bindDelete(elem reflect.Value) (bindInstance, error) {
	plan := t.deletePlan
	if plan.query == "" {

		s := bytes.Buffer{}
		s.WriteString(fmt.Sprintf("delete from %s", t.dbmap.Dialect.QuotedTableForQuery(t.SchemaName, t.TableName)))

		for y := range t.columns {
			col := t.columns[y]
			if !col.Transient {
				if col == t.version {
					plan.versField = col.fieldName
				}
			}
		}

		s.WriteString(" where ")
		for x := range t.keys {
			k := t.keys[x]
			if x > 0 {
				s.WriteString(" and ")
			}
			s.WriteString(t.dbmap.Dialect.QuoteField(k.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))

			plan.keyFields = append(plan.keyFields, k.fieldName)
			plan.argFields = append(plan.argFields, k.fieldName)
		}
		if plan.versField != "" {
			s.WriteString(" and ")
			s.WriteString(t.dbmap.Dialect.QuoteField(t.version.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(len(plan.argFields)))

			plan.argFields = append(plan.argFields, plan.versField)
		}
		s.WriteString(";")

		plan.query = s.String()
		t.deletePlan = plan
	}

	return plan.createBindInstance(elem, t.dbmap.TypeConverter)
}

func (t *TableMap) bindGet() bindPlan {
	plan := t.getPlan
	if plan.query == "" {

		s := bytes.Buffer{}
		s.WriteString("select ")

		x := 0
		for _, col := range t.columns {
			if !col.Transient {
				if x > 0 {
					s.WriteString(",")
				}
				s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
				plan.argFields = append(plan.argFields, col.fieldName)
				x++
			}
		}
		s.WriteString(" from ")
		s.WriteString(t.dbmap.Dialect.QuotedTableForQuery(t.SchemaName, t.TableName))
		s.WriteString(" where ")
		for x := range t.keys {
			col := t.keys[x]
			if x > 0 {
				s.WriteString(" and ")
			}
			s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))

			plan.keyFields = append(plan.keyFields, col.fieldName)
		}
		s.WriteString(";")

		plan.query = s.String()
		t.getPlan = plan
	}

	return plan
}

// Transaction represents a database transaction.
// Insert/Update/Delete/Get/Exec operations will be run in the context
// of that transaction.  Transactions should be terminated with
// a call to Commit() or Rollback()
type Transaction struct {
	dbmap  *DbMap
	tx     *sql.Tx
	closed bool
}

// SqlExecutor exposes gorp operations that can be run from Pre/Post
// hooks.  This hides whether the current operation that triggered the
// hook is in a transaction.
//
// See the DbMap function docs for each of the functions below for more
// information.
type SqlExecutor interface {
	Get(i interface{}, keys ...interface{}) (interface{}, error)
	Insert(list ...interface{}) error
	Update(list ...interface{}) (int64, error)
	Delete(list ...interface{}) (int64, error)
	Exec(query string, args ...interface{}) (sql.Result, error)
	Select(i interface{}, query string, args ...interface{}) ([]interface{}, error)
	SelectInt(query string, args ...interface{}) (int64, error)
	SelectNullInt(query string, args ...interface{}) (sql.NullInt64, error)
	SelectFloat(query string, args ...interface{}) (float64, error)
	SelectNullFloat(query string, args ...interface{}) (sql.NullFloat64, error)
	SelectStr(query string, args ...interface{}) (string, error)
	SelectNullStr(query string, args ...interface{}) (sql.NullString, error)
	SelectOne(holder interface{}, query string, args ...interface{}) error
	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
}

// Compile-time check that DbMap and Transaction implement the SqlExecutor
// interface.
var _, _ SqlExecutor = &DbMap{}, &Transaction{}

type GorpLogger interface {
	Printf(format string, v ...interface{})
}

///////////////

// Insert has the same behavior as DbMap.Insert(), but runs in a transaction.
func (t *Transaction) Insert(list ...interface{}) error {
	return insert(t.dbmap, t, list...)
}

// Update had the same behavior as DbMap.Update(), but runs in a transaction.
func (t *Transaction) Update(list ...interface{}) (int64, error) {
	return update(t.dbmap, t, list...)
}

// Delete has the same behavior as DbMap.Delete(), but runs in a transaction.
func (t *Transaction) Delete(list ...interface{}) (int64, error) {
	return delete(t.dbmap, t, list...)
}

// Get has the same behavior as DbMap.Get(), but runs in a transaction.
func (t *Transaction) Get(i interface{}, keys ...interface{}) (interface{}, error) {
	return get(t.dbmap, t, i, keys...)
}

// Select has the same behavior as DbMap.Select(), but runs in a transaction.
func (t *Transaction) Select(i interface{}, query string, args ...interface{}) ([]interface{}, error) {
	return hookedselect(t.dbmap, t, i, query, args...)
}

// Exec has the same behavior as DbMap.Exec(), but runs in a transaction.
func (t *Transaction) Exec(query string, args ...interface{}) (sql.Result, error) {
	t.dbmap.trace(query, args...)
	return t.tx.Exec(query, args...)
}

// SelectInt is a convenience wrapper around the gorp.SelectInt function.
func (t *Transaction) SelectInt(query string, args ...interface{}) (int64, error) {
	return SelectInt(t, query, args...)
}

// SelectNullInt is a convenience wrapper around the gorp.SelectNullInt function.
func (t *Transaction) SelectNullInt(query string, args ...interface{}) (sql.NullInt64, error) {
	return SelectNullInt(t, query, args...)
}

// SelectFloat is a convenience wrapper around the gorp.SelectFloat function.
func (t *Transaction) SelectFloat(query string, args ...interface{}) (float64, error) {
	return SelectFloat(t, query, args...)
}

// SelectNullFloat is a convenience wrapper around the gorp.SelectNullFloat function.
func (t *Transaction) SelectNullFloat(query string, args ...interface{}) (sql.NullFloat64, error) {
	return SelectNullFloat(t, query, args...)
}

// SelectStr is a convenience wrapper around the gorp.SelectStr function.
func (t *Transaction) SelectStr(query string, args ...interface{}) (string, error) {
	return SelectStr(t, query, args...)
}

// SelectNullStr is a convenience wrapper around the gorp.SelectNullStr function.
func (t *Transaction) SelectNullStr(query string, args ...interface{}) (sql.NullString, error) {
	return SelectNullStr(t, query, args...)
}

// SelectOne is a convenience wrapper around the gorp.SelectOne function.
func (t *Transaction) SelectOne(holder interface{}, query string, args ...interface{}) error {
	return SelectOne(t.dbmap, t, holder, query, args...)
}

// Commit commits the underlying database transaction.
func (t *Transaction) Commit() error {
	if !t.closed {
		t.closed = true
		t.dbmap.trace("commit;")
		return t.tx.Commit()
	}

	return sql.ErrTxDone
}

// Rollback rolls back the underlying database transaction.
func (t *Transaction) Rollback() error {
	if !t.closed {
		t.closed = true
		t.dbmap.trace("rollback;")
		return t.tx.Rollback()
	}

	return sql.ErrTxDone
}

// Savepoint creates a savepoint with the given name. The name is interpolated
// directly into the SQL SAVEPOINT statement, so you must sanitize it if it is
// derived from user input.
func (t *Transaction) Savepoint(name string) error {
	query := "savepoint " + t.dbmap.Dialect.QuoteField(name)
	t.dbmap.trace(query, nil)
	_, err := t.tx.Exec(query)
	return err
}

// RollbackToSavepoint rolls back to the savepoint with the given name. The
// name is interpolated directly into the SQL SAVEPOINT statement, so you must
// sanitize it if it is derived from user input.
func (t *Transaction) RollbackToSavepoint(savepoint string) error {
	query := "rollback to savepoint " + t.dbmap.Dialect.QuoteField(savepoint)
	t.dbmap.trace(query, nil)
	_, err := t.tx.Exec(query)
	return err
}

// ReleaseSavepint releases the savepoint with the given name. The name is
// interpolated directly into the SQL SAVEPOINT statement, so you must sanitize
// it if it is derived from user input.
func (t *Transaction) ReleaseSavepoint(savepoint string) error {
	query := "release savepoint " + t.dbmap.Dialect.QuoteField(savepoint)
	t.dbmap.trace(query, nil)
	_, err := t.tx.Exec(query)
	return err
}

func (t *Transaction) QueryRow(query string, args ...interface{}) *sql.Row {
	t.dbmap.trace(query, args...)
	return t.tx.QueryRow(query, args...)
}

func (t *Transaction) Query(query string, args ...interface{}) (*sql.Rows, error) {
	t.dbmap.trace(query, args...)
	return t.tx.Query(query, args...)
}

///////////////

// SelectInt executes the given query, which should be a SELECT statement for a single
// integer column, and returns the value of the first row returned.  If no rows are
// found, zero is returned.
func SelectInt(e SqlExecutor, query string, args ...interface{}) (int64, error) {
	var h int64
	err := selectVal(e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return h, nil
}

// SelectNullInt executes the given query, which should be a SELECT statement for a single
// integer column, and returns the value of the first row returned.  If no rows are
// found, the empty sql.NullInt64 value is returned.
func SelectNullInt(e SqlExecutor, query string, args ...interface{}) (sql.NullInt64, error) {
	var h sql.NullInt64
	err := selectVal(e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return h, err
	}
	return h, nil
}

// SelectFloat executes the given query, which should be a SELECT statement for a single
// float column, and returns the value of the first row returned. If no rows are
// found, zero is returned.
func SelectFloat(e SqlExecutor, query string, args ...interface{}) (float64, error) {
	var h float64
	err := selectVal(e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return h, nil
}

// SelectNullFloat executes the given query, which should be a SELECT statement for a single
// float column, and returns the value of the first row returned. If no rows are
// found, the empty sql.NullInt64 value is returned.
func SelectNullFloat(e SqlExecutor, query string, args ...interface{}) (sql.NullFloat64, error) {
	var h sql.NullFloat64
	err := selectVal(e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return h, err
	}
	return h, nil
}

// SelectStr executes the given query, which should be a SELECT statement for a single
// char/varchar column, and returns the value of the first row returned.  If no rows are
// found, an empty string is returned.
func SelectStr(e SqlExecutor, query string, args ...interface{}) (string, error) {
	var h string
	err := selectVal(e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	return h, nil
}

// SelectNullStr executes the given query, which should be a SELECT
// statement for a single char/varchar column, and returns the value
// of the first row returned.  If no rows are found, the empty
// sql.NullString is returned.
func SelectNullStr(e SqlExecutor, query string, args ...interface{}) (sql.NullString, error) {
	var h sql.NullString
	err := selectVal(e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return h, err
	}
	return h, nil
}

// SelectOne executes the given query (which should be a SELECT statement)
// and binds the result to holder, which must be a pointer.
//
// If no row is found, an an error (sql.ErrNoRows specifically) will be returned
//
// If more than one row is found, an error will be returned.
//
func SelectOne(m *DbMap, e SqlExecutor, holder interface{}, query string, args ...interface{}) error {
	t := reflect.TypeOf(holder)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	} else {
		return fmt.Errorf("gorp: SelectOne holder must be a pointer, but got: %t", holder)
	}

	if t.Kind() == reflect.Struct {
		list, err := hookedselect(m, e, holder, query, args...)
		if err != nil {
			return err
		}

		dest := reflect.ValueOf(holder)

		if list != nil && len(list) > 0 {
			// check for multiple rows
			if len(list) > 1 {
				return fmt.Errorf("gorp: multiple rows returned for: %s - %v", query, args)
			}

			// only one row found
			src := reflect.ValueOf(list[0])
			dest.Elem().Set(src.Elem())
		} else {
			// No rows found, return a proper error.
			return sql.ErrNoRows
		}

		return nil
	}

	return selectVal(e, holder, query, args...)
}

func selectVal(e SqlExecutor, holder interface{}, query string, args ...interface{}) error {
	if len(args) == 1 {
		switch m := e.(type) {
		case *DbMap:
			query, args = maybeExpandNamedQuery(m, query, args)
		case *Transaction:
			query, args = maybeExpandNamedQuery(m.dbmap, query, args)
		}
	}
	rows, err := e.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	if !rows.Next() {
		return sql.ErrNoRows
	}

	return rows.Scan(holder)
}

///////////////

func hookedselect(m *DbMap, exec SqlExecutor, i interface{}, query string,
	args ...interface{}) ([]interface{}, error) {

	list, err := rawselect(m, exec, i, query, args...)
	if err != nil {
		return nil, err
	}

	// Determine where the results are: written to i, or returned in list
	if t, _ := toSliceType(i); t == nil {
		for _, v := range list {
			err = runHook("PostGet", reflect.ValueOf(v), hookArg(exec))
			if err != nil {
				return nil, err
			}
		}
	} else {
		resultsValue := reflect.Indirect(reflect.ValueOf(i))
		for i := 0; i < resultsValue.Len(); i++ {
			err = runHook("PostGet", resultsValue.Index(i), hookArg(exec))
			if err != nil {
				return nil, err
			}
		}
	}
	return list, nil
}

func rawselect(m *DbMap, exec SqlExecutor, i interface{}, query string,
	args ...interface{}) ([]interface{}, error) {
	var (
		appendToSlice   = false // Write results to i directly?
		intoStruct      = true  // Selecting into a struct?
		pointerElements = true  // Are the slice elements pointers (vs values)?
	)

	// get type for i, verifying it's a supported destination
	t, err := toType(i)
	if err != nil {
		var err2 error
		if t, err2 = toSliceType(i); t == nil {
			if err2 != nil {
				return nil, err2
			}
			return nil, err
		}
		pointerElements = t.Kind() == reflect.Ptr
		if pointerElements {
			t = t.Elem()
		}
		appendToSlice = true
		intoStruct = t.Kind() == reflect.Struct
	}

	// If the caller supplied a single struct/map argument, assume a "named
	// parameter" query.  Extract the named arguments from the struct/map, create
	// the flat arg slice, and rewrite the query to use the dialect's placeholder.
	if len(args) == 1 {
		query, args = maybeExpandNamedQuery(m, query, args)
	}

	// Run the query
	rows, err := exec.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Fetch the column names as returned from db
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	if !intoStruct && len(cols) > 1 {
		return nil, fmt.Errorf("gorp: select into non-struct slice requires 1 column, got %d", len(cols))
	}

	var colToFieldIndex [][]int
	if intoStruct {
		if colToFieldIndex, err = columnToFieldIndex(m, t, cols); err != nil {
			return nil, err
		}
	}

	conv := m.TypeConverter

	// Add results to one of these two slices.
	var (
		list       = make([]interface{}, 0)
		sliceValue = reflect.Indirect(reflect.ValueOf(i))
	)

	for {
		if !rows.Next() {
			// if error occured return rawselect
			if rows.Err() != nil {
				return nil, rows.Err()
			}
			// time to exit from outer "for" loop
			break
		}
		v := reflect.New(t)
		dest := make([]interface{}, len(cols))

		custScan := make([]CustomScanner, 0)

		for x := range cols {
			f := v.Elem()
			if intoStruct {
				f = f.FieldByIndex(colToFieldIndex[x])
			}
			target := f.Addr().Interface()
			if conv != nil {
				scanner, ok := conv.FromDb(target)
				if ok {
					target = scanner.Holder
					custScan = append(custScan, scanner)
				}
			}
			dest[x] = target
		}

		err = rows.Scan(dest...)
		if err != nil {
			return nil, err
		}

		for _, c := range custScan {
			err = c.Bind()
			if err != nil {
				return nil, err
			}
		}

		if appendToSlice {
			if !pointerElements {
				v = v.Elem()
			}
			sliceValue.Set(reflect.Append(sliceValue, v))
		} else {
			list = append(list, v.Interface())
		}
	}

	if appendToSlice && sliceValue.IsNil() {
		sliceValue.Set(reflect.MakeSlice(sliceValue.Type(), 0, 0))
	}

	return list, nil
}

// maybeExpandNamedQuery checks the given arg to see if it's eligible to be used
// as input to a named query.  If so, it rewrites the query to use
// dialect-dependent bindvars and instantiates the corresponding slice of
// parameters by extracting data from the map / struct.
// If not, returns the input values unchanged.
func maybeExpandNamedQuery(m *DbMap, query string, args []interface{}) (string, []interface{}) {
	arg := reflect.ValueOf(args[0])
	for arg.Kind() == reflect.Ptr {
		arg = arg.Elem()
	}
	switch {
	case arg.Kind() == reflect.Map && arg.Type().Key().Kind() == reflect.String:
		return expandNamedQuery(m, query, func(key string) reflect.Value {
				return arg.MapIndex(reflect.ValueOf(key))
			})
		// #84 - ignore time.Time structs here - there may be a cleaner way to do this
	case arg.Kind() == reflect.Struct && !(arg.Type().PkgPath() == "time" && arg.Type().Name() == "Time"):
		return expandNamedQuery(m, query, arg.FieldByName)
	}
	return query, args
}

var keyRegexp = regexp.MustCompile(`:[[:word:]]+`)

// expandNamedQuery accepts a query with placeholders of the form ":key", and a
// single arg of Kind Struct or Map[string].  It returns the query with the
// dialect's placeholders, and a slice of args ready for positional insertion
// into the query.
func expandNamedQuery(m *DbMap, query string, keyGetter func(key string) reflect.Value) (string, []interface{}) {
	var (
		n    int
		args []interface{}
	)
	return keyRegexp.ReplaceAllStringFunc(query, func(key string) string {
			val := keyGetter(key[1:])
			if !val.IsValid() {
				return key
			}
			args = append(args, val.Interface())
			newVar := m.Dialect.BindVar(n)
			n++
			return newVar
		}), args
}

func columnToFieldIndex(m *DbMap, t reflect.Type, cols []string) ([][]int, error) {
	colToFieldIndex := make([][]int, len(cols))

	// check if type t is a mapped table - if so we'll
	// check the table for column aliasing below
	tableMapped := false
	table := tableOrNil(m, t)
	if table != nil {
		tableMapped = true
	}

	// Loop over column names and find field in i to bind to
	// based on column name. all returned columns must match
	// a field in the i struct
	for x := range cols {
		colName := strings.ToLower(cols[x])

		field, found := t.FieldByNameFunc(func(fieldName string) bool {
			field, _ := t.FieldByName(fieldName)
			fieldName = field.Tag.Get("db")

			if fieldName == "-" {
				return false
			} else if fieldName == "" {
				fieldName = field.Name
			}
			if tableMapped {
				colMap := colMapOrNil(table, fieldName)
				if colMap != nil {
					fieldName = colMap.ColumnName
				}
			}

			return colName == strings.ToLower(fieldName)
		})
		if found {
			colToFieldIndex[x] = field.Index
		}
		if colToFieldIndex[x] == nil {
			return nil, fmt.Errorf("gorp: No field %s in type %s", colName, t.Name())
		}
	}
	return colToFieldIndex, nil
}

func fieldByName(val reflect.Value, fieldName string) *reflect.Value {
	// try to find field by exact match
	f := val.FieldByName(fieldName)

	if f != zeroVal {
		return &f
	}

	// try to find by case insensitive match - only the Postgres driver
	// seems to require this - in the case where columns are aliased in the sql
	fieldNameL := strings.ToLower(fieldName)
	fieldCount := val.NumField()
	t := val.Type()
	for i := 0; i < fieldCount; i++ {
		sf := t.Field(i)
		if strings.ToLower(sf.Name) == fieldNameL {
			f := val.Field(i)
			return &f
		}
	}

	return nil
}

// toSliceType returns the element type of the given object, if the object is a
// "*[]*Element" or "*[]Element". If not, returns nil.
// err is returned if the user was trying to pass a pointer-to-slice but failed.
func toSliceType(i interface{}) (reflect.Type, error) {
	t := reflect.TypeOf(i)
	if t.Kind() != reflect.Ptr {
		// If it's a slice, return a more helpful error message
		if t.Kind() == reflect.Slice {
			return nil, fmt.Errorf("gorp: Cannot SELECT into a non-pointer slice: %v", t)
		}
		return nil, nil
	}
	if t = t.Elem(); t.Kind() != reflect.Slice {
		return nil, nil
	}
	return t.Elem(), nil
}

func toType(i interface{}) (reflect.Type, error) {
	t := reflect.TypeOf(i)

	// If a Pointer to a type, follow
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("gorp: Cannot SELECT into this type: %v", reflect.TypeOf(i))
	}
	return t, nil
}

func get(m *DbMap, exec SqlExecutor, i interface{},
	keys ...interface{}) (interface{}, error) {

	t, err := toType(i)
	if err != nil {
		return nil, err
	}

	table, err := m.tableFor(t, true)
	if err != nil {
		return nil, err
	}

	plan := table.bindGet()

	v := reflect.New(t)
	dest := make([]interface{}, len(plan.argFields))

	conv := m.TypeConverter
	custScan := make([]CustomScanner, 0)

	for x, fieldName := range plan.argFields {
		f := v.Elem().FieldByName(fieldName)
		target := f.Addr().Interface()
		if conv != nil {
			scanner, ok := conv.FromDb(target)
			if ok {
				target = scanner.Holder
				custScan = append(custScan, scanner)
			}
		}
		dest[x] = target
	}

	row := exec.QueryRow(plan.query, keys...)
	err = row.Scan(dest...)
	if err != nil {
		if err == sql.ErrNoRows {
			err = nil
		}
		return nil, err
	}

	for _, c := range custScan {
		err = c.Bind()
		if err != nil {
			return nil, err
		}
	}

	err = runHook("PostGet", v, hookArg(exec))
	if err != nil {
		return nil, err
	}

	return v.Interface(), nil
}

func delete(m *DbMap, exec SqlExecutor, list ...interface{}) (int64, error) {
	hookarg := hookArg(exec)
	count := int64(0)
	for _, ptr := range list {
		table, elem, err := m.tableForPointer(ptr, true)
		if err != nil {
			return -1, err
		}

		eptr := elem.Addr()
		err = runHook("PreDelete", eptr, hookarg)
		if err != nil {
			return -1, err
		}

		bi, err := table.bindDelete(elem)
		if err != nil {
			return -1, err
		}

		res, err := exec.Exec(bi.query, bi.args...)
		if err != nil {
			return -1, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return -1, err
		}

		if rows == 0 && bi.existingVersion > 0 {
			return lockError(m, exec, table.TableName,
				bi.existingVersion, elem, bi.keys...)
		}

		count += rows

		err = runHook("PostDelete", eptr, hookarg)
		if err != nil {
			return -1, err
		}
	}

	return count, nil
}

func update(m *DbMap, exec SqlExecutor, list ...interface{}) (int64, error) {
	hookarg := hookArg(exec)
	count := int64(0)
	for _, ptr := range list {
		table, elem, err := m.tableForPointer(ptr, true)
		if err != nil {
			return -1, err
		}

		eptr := elem.Addr()
		err = runHook("PreUpdate", eptr, hookarg)
		if err != nil {
			return -1, err
		}

		bi, err := table.bindUpdate(elem)
		if err != nil {
			return -1, err
		}

		res, err := exec.Exec(bi.query, bi.args...)
		if err != nil {
			return -1, err
		}

		rows, err := res.RowsAffected()
		if err != nil {
			return -1, err
		}

		if rows == 0 && bi.existingVersion > 0 {
			return lockError(m, exec, table.TableName,
				bi.existingVersion, elem, bi.keys...)
		}

		if bi.versField != "" {
			elem.FieldByName(bi.versField).SetInt(bi.existingVersion + 1)
		}

		count += rows

		err = runHook("PostUpdate", eptr, hookarg)
		if err != nil {
			return -1, err
		}
	}
	return count, nil
}

func insert(m *DbMap, exec SqlExecutor, list ...interface{}) error {
	hookarg := hookArg(exec)
	for _, ptr := range list {
		table, elem, err := m.tableForPointer(ptr, false)
		if err != nil {
			return err
		}

		eptr := elem.Addr()
		err = runHook("PreInsert", eptr, hookarg)
		if err != nil {
			return err
		}

		bi, err := table.bindInsert(elem)
		if err != nil {
			return err
		}

		if bi.autoIncrIdx > -1 {
			id, err := m.Dialect.InsertAutoIncr(exec, bi.query, bi.args...)
			if err != nil {
				return err
			}
			f := elem.FieldByName(bi.autoIncrFieldName)
			k := f.Kind()
			if (k == reflect.Int) || (k == reflect.Int16) || (k == reflect.Int32) || (k == reflect.Int64) {
				f.SetInt(id)
			} else if (k == reflect.Uint16) || (k == reflect.Uint32) || (k == reflect.Uint64) {
				f.SetUint(uint64(id))
			} else {
				return fmt.Errorf("gorp: Cannot set autoincrement value on non-Int field. SQL=%s  autoIncrIdx=%d autoIncrFieldName=%s", bi.query, bi.autoIncrIdx, bi.autoIncrFieldName)
			}
		} else {
			_, err := exec.Exec(bi.query, bi.args...)
			if err != nil {
				return err
			}
		}

		err = runHook("PostInsert", eptr, hookarg)
		if err != nil {
			return err
		}
	}
	return nil
}

func hookArg(exec SqlExecutor) []reflect.Value {
	execval := reflect.ValueOf(exec)
	return []reflect.Value{execval}
}

func runHook(name string, eptr reflect.Value, arg []reflect.Value) error {
	hook := eptr.MethodByName(name)
	if hook != zeroVal {
		ret := hook.Call(arg)
		if len(ret) > 0 && !ret[0].IsNil() {
			return ret[0].Interface().(error)
		}
	}
	return nil
}

func lockError(m *DbMap, exec SqlExecutor, tableName string,
	existingVer int64, elem reflect.Value,
	keys ...interface{}) (int64, error) {

	existing, err := get(m, exec, elem.Interface(), keys...)
	if err != nil {
		return -1, err
	}

	ole := OptimisticLockError{tableName, keys, true, existingVer}
	if existing == nil {
		ole.RowExists = false
	}
	return -1, ole
}
