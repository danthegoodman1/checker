package migrations

import (
	"database/sql"
	"embed"
	"fmt"

	// ensure "pgx" driver is loaded
	"github.com/danthegoodman1/checker/gologger"
	_ "github.com/jackc/pgx/v5/stdlib"
	migrate "github.com/rubenv/sql-migrate"
)

var (
	//go:embed *.sql
	migrations embed.FS

	ErrMigrationsNotRun = fmt.Errorf("not all migrations applied")

	logger = gologger.NewLogger()
)

func RunMigrations(pgDSN string) (int, error) {
	db, err := sql.Open("pgx", pgDSN)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	src := migrate.EmbedFileSystemMigrationSource{
		FileSystem: migrations,
		Root:       ".",
	}
	ms := migrate.MigrationSet{
		TableName: "migrations",
	}
	return ms.Exec(db, "postgres", src, migrate.Up)
}

func CheckMigrations(pgDSN string) error {
	db, err := sql.Open("pgx", pgDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	src := migrate.EmbedFileSystemMigrationSource{
		FileSystem: migrations,
		Root:       ".",
	}
	ms := migrate.MigrationSet{
		TableName: "migrations",
	}
	migration, _, err := ms.PlanMigration(db, "postgres", src, migrate.Up, 0)
	if err != nil {
		return err
	}
	if len(migration) > 0 {
		for _, mig := range migration {
			logger.Warn().Str("migrationID", mig.Id).Msg("missing migration")
		}
		return ErrMigrationsNotRun
	}
	return nil
}
