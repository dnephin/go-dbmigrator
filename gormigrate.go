package migrator

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

const initSchemaMigrationID = "SCHEMA_INIT"

// Options define options for all migrations.
type Options struct {
	// TableName is the migration table.
	TableName string
	// IDColumnName is the name of column where the migration id will be stored.
	IDColumnName string
	// IDColumnSize is the length of the migration id column
	IDColumnSize int
	// UseTransaction makes Migrator execute migrations inside a single transaction.
	// Keep in mind that not all databases support DDL commands inside transactions.
	UseTransaction bool

	// InitSchema is used to create the database when no migrations table exists.
	// This function should create all tables, and constraints. After this
	// function is run, migrator will create the migrations table and populate
	// it with the IDs of all the currently defined migrations.
	InitSchema func(*gorm.DB) error
}

// Migration represents a database migration (a modification to be made on the database).
type Migration struct {
	// ID is the migration identifier. Usually a timestamp like "201601021504".
	ID string
	// Migrate is a function that will br executed while running this migration.
	Migrate func(*gorm.DB) error
	// Rollback will be executed on rollback. Can be nil.
	Rollback func(*gorm.DB) error
}

// Migrator represents a collection of all migrations of a database schema.
type Migrator struct {
	db         *gorm.DB
	tx         *gorm.DB
	options    Options
	migrations []*Migration
}

// DefaultOptions can be used if you don't want to think about options.
var DefaultOptions = Options{
	TableName:      "migrations",
	IDColumnName:   "id",
	IDColumnSize:   255,
	UseTransaction: false,
}

// New returns a new Migrator.
func New(db *gorm.DB, options Options, migrations []*Migration) *Migrator {
	if options.TableName == "" {
		options.TableName = DefaultOptions.TableName
	}
	if options.IDColumnName == "" {
		options.IDColumnName = DefaultOptions.IDColumnName
	}
	if options.IDColumnSize == 0 {
		options.IDColumnSize = DefaultOptions.IDColumnSize
	}
	return &Migrator{
		db:         db,
		options:    options,
		migrations: migrations,
	}
}

// Migrate executes all migrations that did not run yet.
func (g *Migrator) Migrate() error {
	if g.options.InitSchema == nil && len(g.migrations) == 0 {
		return fmt.Errorf("there are no migrations")
	}

	if err := g.validate(); err != nil {
		return err
	}

	rollback := g.begin()
	defer rollback()

	// TODO: do this in shouldInitializeSchema instead
	if err := g.createMigrationTableIfNotExists(); err != nil {
		return err
	}

	if g.options.InitSchema != nil { // TODO: remove this if
		canInitializeSchema, err := g.shouldInitializeSchema()
		if err != nil {
			return err
		}
		if canInitializeSchema {
			if err := g.runInitSchema(); err != nil {
				return err
			}
			return g.commit()
		}
	}

	for _, migration := range g.migrations {
		if err := g.runMigration(migration); err != nil {
			return err
		}
	}
	return g.commit()
}

func (g *Migrator) validate() error {
	lookup := make(map[string]struct{}, len(g.migrations))
	ids := make([]string, len(g.migrations))

	for _, m := range g.migrations {
		switch m.ID {
		case "":
			return fmt.Errorf("migration is missing an ID")
		case initSchemaMigrationID:
			return fmt.Errorf("migration can not use reserved ID: %v", m.ID)
		}
		if _, ok := lookup[m.ID]; ok {
			return fmt.Errorf("duplicate migration ID: %v", m.ID)
		}
		lookup[m.ID] = struct{}{}
		ids = append(ids, m.ID)
	}
	return nil
}

func (g *Migrator) checkIDExist(migrationID string) error {
	if migrationID == initSchemaMigrationID {
		return nil
	}
	for _, migrate := range g.migrations {
		if migrate.ID == migrationID {
			return nil
		}
	}
	return fmt.Errorf("migration ID %v does not exist", migrationID)
}

// RollbackTo undoes migrations up to the given migration that matches the `migrationID`.
// Migration with the matching `migrationID` is not rolled back.
func (g *Migrator) RollbackTo(migrationID string) error {
	if len(g.migrations) == 0 {
		return fmt.Errorf("there are no migrations")
	}

	if err := g.checkIDExist(migrationID); err != nil {
		return err
	}

	rollback := g.begin()
	defer rollback()

	for i := len(g.migrations) - 1; i >= 0; i-- {
		migration := g.migrations[i]
		if migration.ID == migrationID {
			break
		}
		switch migrationRan, err := g.migrationRan(migration); {
		case err != nil:
			return err
		case migrationRan:
			if err := g.rollbackMigration(migration); err != nil {
				return err
			}
		}
	}
	return g.commit()
}

func (g *Migrator) rollbackMigration(m *Migration) error {
	if m.Rollback == nil {
		return errors.New("migration can not be rollback back")
	}

	if err := m.Rollback(g.tx); err != nil {
		return err
	}

	// TODO: use queryBuilder or hardcode table and column names
	sql := fmt.Sprintf("DELETE FROM %s WHERE %s = ?", g.options.TableName, g.options.IDColumnName)
	return g.tx.Exec(sql, m.ID).Error
}

func (g *Migrator) runInitSchema() error {
	if err := g.options.InitSchema(g.tx); err != nil {
		return err
	}
	if err := g.insertMigration(initSchemaMigrationID); err != nil {
		return err
	}
	for _, migration := range g.migrations {
		if err := g.insertMigration(migration.ID); err != nil {
			return err
		}
	}
	return nil
}

func (g *Migrator) runMigration(migration *Migration) error {
	switch migrationRan, err := g.migrationRan(migration); {
	case err != nil:
		return err
	case migrationRan:
		return nil
	}

	if err := migration.Migrate(g.tx); err != nil {
		return err
	}
	return g.insertMigration(migration.ID)
}

func (g *Migrator) createMigrationTableIfNotExists() error {
	// TODO: replace gorm helper
	if g.tx.Migrator().HasTable(g.options.TableName) {
		return nil
	}

	// TODO: use queryBuilder or hardcode table and column names
	sql := fmt.Sprintf("CREATE TABLE %s (%s VARCHAR(%d) PRIMARY KEY)", g.options.TableName, g.options.IDColumnName, g.options.IDColumnSize)
	return g.tx.Exec(sql).Error
}

// TODO: select all values from the table once, instead of selecting each
// individually
func (g *Migrator) migrationRan(m *Migration) (bool, error) {
	var count int64
	err := g.tx.
		Table(g.options.TableName).
		Where(fmt.Sprintf("%s = ?", g.options.IDColumnName), m.ID).
		Count(&count).
		Error
	return count > 0, err
}

func (g *Migrator) shouldInitializeSchema() (bool, error) {
	migrationRan, err := g.migrationRan(&Migration{ID: initSchemaMigrationID})
	if err != nil {
		return false, err
	}
	if migrationRan {
		return false, nil
	}

	// If the ID doesn't exist, we also want the list of migrations to be empty
	var count int64
	err = g.tx.
		Table(g.options.TableName).
		Count(&count).
		Error
	return count == 0, err
}

func (g *Migrator) insertMigration(id string) error {
	// TODO: use queryBuilder, or hardcode table and column name
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (?)", g.options.TableName, g.options.IDColumnName)
	return g.tx.Exec(sql, id).Error
}

func (g *Migrator) begin() func() {
	if g.options.UseTransaction {
		g.tx = g.db.Begin()
		return func() {
			g.tx.Rollback()
		}
	}
	g.tx = g.db
	return func() {}
}

func (g *Migrator) commit() error {
	if g.options.UseTransaction {
		return g.tx.Commit().Error
	}
	return nil
}
