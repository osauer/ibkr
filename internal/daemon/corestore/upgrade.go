package corestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Inspect validates an existing authority without migrating, repairing, or
// opening it for writes. A supported older version is reported as
// InspectionUpgradeRequired rather than treated as corruption.
func Inspect(ctx context.Context, opts InspectOptions) (Inspection, error) {
	return inspectWithPlan(ctx, opts, currentMigrationPlan())
}

func readSchemaVersionOnly(ctx context.Context, path string, busy time.Duration) (int, error) {
	before, err := snapshotReadOnlySidecars(path)
	if err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, busy, true))
	if err != nil {
		return 0, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return 0, errors.Join(err, cleanupNewReadOnlySidecars(path, before))
	}
	var version int
	queryErr := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version)
	closeErr := db.Close()
	cleanupErr := cleanupNewReadOnlySidecars(path, before)
	if queryErr != nil {
		return 0, queryErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if cleanupErr != nil {
		return 0, cleanupErr
	}
	return version, nil
}

func inspectWithPlan(ctx context.Context, opts InspectOptions, plan []migration) (Inspection, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return Inspection{}, err
	}
	path, _, err := existingRegularPath(opts.Path, "authority")
	if err != nil {
		return Inspection{}, err
	}
	before, err := snapshotReadOnlySidecars(path)
	if err != nil {
		return Inspection{}, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, true))
	if err != nil {
		return Inspection{}, fmt.Errorf("open authority for inspection: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return Inspection{}, fmt.Errorf("open authority for inspection: %w", errors.Join(err, cleanupNewReadOnlySidecars(path, before)))
	}
	inspection, inspectErr := inspectDBWithPlan(ctx, db, path, opts.MinimumHead, plan)
	closeErr := db.Close()
	cleanupErr := cleanupNewReadOnlySidecars(path, before)
	if inspectErr != nil {
		return Inspection{}, inspectErr
	}
	if closeErr != nil {
		return Inspection{}, fmt.Errorf("close inspected authority: %w", closeErr)
	}
	if cleanupErr != nil {
		return Inspection{}, cleanupErr
	}
	return inspection, nil
}

type readOnlySidecarSnapshot map[string]bool

func snapshotReadOnlySidecars(path string) (readOnlySidecarSnapshot, error) {
	snapshot := make(readOnlySidecarSnapshot, 3)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect authority%s: %w", suffix, err)
		}
		if err := validatePrivateRegularInfo(info, "authority"+suffix); err != nil {
			return nil, err
		}
		snapshot[suffix] = true
	}
	return snapshot, nil
}

func cleanupNewReadOnlySidecars(path string, before readOnlySidecarSnapshot) error {
	removed := false
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if before[suffix] {
			continue
		}
		candidate := path + suffix
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect read-only authority%s: %w", suffix, err)
		}
		if err := validatePrivateRegularInfo(info, "read-only authority"+suffix); err != nil {
			return err
		}
		if suffix != "-shm" && info.Size() != 0 {
			return fmt.Errorf("corestore: read-only inspection created non-empty %s sidecar", suffix)
		}
		if err := os.Remove(candidate); err != nil {
			return fmt.Errorf("remove read-only authority%s: %w", suffix, err)
		}
		removed = true
	}
	if removed {
		return syncDir(filepath.Dir(path))
	}
	return nil
}

func inspectDBWithPlan(ctx context.Context, db *sql.DB, path string, minimum *AuthorityHead, plan []migration) (Inspection, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return Inspection{}, err
	}
	target := len(plan)
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return Inspection{}, fmt.Errorf("read schema version: %w", err)
	}
	if version > target {
		return Inspection{}, fmt.Errorf("future schema version %d exceeds supported %d", version, target)
	}
	if version < 1 {
		return Inspection{}, errorsf("unmanaged or incomplete authority database")
	}
	if err := validateSchemaLedgerWithPlan(ctx, db, version, plan); err != nil {
		return Inspection{}, fmt.Errorf("validate authority schema version %d: %w", version, err)
	}
	report, err := checkIntegrityDB(ctx, db)
	if err != nil {
		return Inspection{}, fmt.Errorf("inspect authority integrity: %w", err)
	}
	if !report.OK() {
		return Inspection{}, integrityFailure(report)
	}
	head, err := readAuthorityHead(ctx, db)
	if err != nil {
		return Inspection{}, fmt.Errorf("read authority head: %w", err)
	}
	if minimum != nil {
		if err := requireMinimumHead(head, *minimum); err != nil {
			return Inspection{}, err
		}
	}
	status := InspectionCurrent
	if version < target {
		status = InspectionUpgradeRequired
	}
	return Inspection{
		Path: path, SchemaVersion: version, TargetVersion: target,
		Status: status, Head: head, Integrity: report,
	}, nil
}

// PrepareUpgrade creates an immutable exact-head backup and an independently
// validated target-version candidate. It never changes SourcePath and never
// publishes CandidatePath over an existing file unless ReplaceCandidate is
// explicitly set for crash recovery.
func PrepareUpgrade(ctx context.Context, opts UpgradeOptions) (UpgradeResult, error) {
	return prepareUpgradeWithPlan(ctx, opts, currentMigrationPlan())
}

// QuiesceForReplacement checkpoints the exact validated old authority and
// removes only disposable SQLite sidecars. The caller must hold the daemon's
// state-root persistence lock and have closed every Store handle for this path
// for the full call through atomic replacement. It is intentionally separate
// from PrepareUpgrade because this is the one step that may change SourcePath's
// physical representation, and belongs immediately before atomic publication.
func QuiesceForReplacement(ctx context.Context, opts QuiesceOptions) (Inspection, error) {
	plan := currentMigrationPlan()
	if opts.ExpectedSchemaVersion < 1 || opts.ExpectedSchemaVersion > len(plan) {
		return Inspection{}, errorsf("unsupported expected schema version for replacement")
	}
	path, _, err := existingRegularPath(opts.Path, "replacement source")
	if err != nil {
		return Inspection{}, err
	}
	if err := validateReplacementSidecarTypes(path); err != nil {
		return Inspection{}, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, false))
	if err != nil {
		return Inspection{}, fmt.Errorf("open replacement source: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return Inspection{}, fmt.Errorf("open replacement source: %w", err)
	}
	expected := opts.ExpectedHead
	before, err := inspectDBWithPlan(ctx, db, path, &expected, plan)
	if err != nil {
		_ = db.Close()
		return Inspection{}, err
	}
	if before.SchemaVersion != opts.ExpectedSchemaVersion || before.Head != opts.ExpectedHead {
		_ = db.Close()
		return Inspection{}, fmt.Errorf("%w: replacement source does not match expected version and head", ErrRollback)
	}
	var checkpoint CheckpointResult
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&checkpoint.Busy, &checkpoint.LogFrames, &checkpoint.CheckpointedFrames); err != nil {
		_ = db.Close()
		return Inspection{}, fmt.Errorf("checkpoint replacement source: %w", err)
	}
	if checkpoint.Busy != 0 {
		_ = db.Close()
		return Inspection{}, ErrCheckpointBusy
	}
	if err := db.Close(); err != nil {
		return Inspection{}, fmt.Errorf("close replacement source: %w", err)
	}
	if err := removeQuiescedSidecars(path); err != nil {
		return Inspection{}, err
	}
	if err := syncFile(path); err != nil {
		return Inspection{}, fmt.Errorf("sync replacement source: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return Inspection{}, err
	}
	after, err := inspectWithPlan(ctx, InspectOptions{Path: path, MinimumHead: &expected}, plan)
	if err != nil {
		return Inspection{}, err
	}
	if after.SchemaVersion != opts.ExpectedSchemaVersion || after.Head != opts.ExpectedHead {
		return Inspection{}, fmt.Errorf("%w: replacement source changed during checkpoint", ErrRollback)
	}
	return after, nil
}

func removeQuiescedSidecars(path string) error {
	walPath := path + "-wal"
	if info, err := os.Lstat(walPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errorsf("replacement WAL sidecar must be a regular file, not a symbolic link")
		}
		if err := validatePrivateRegularInfo(info, "replacement WAL sidecar"); err != nil {
			return err
		}
		if info.Size() != 0 {
			return errorsf("replacement WAL sidecar is not empty after checkpoint")
		}
		if err := os.Remove(walPath); err != nil {
			return fmt.Errorf("remove empty replacement WAL: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect replacement WAL: %w", err)
	}
	shmPath := path + "-shm"
	if info, err := os.Lstat(shmPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errorsf("replacement SHM sidecar must be a regular file, not a symbolic link")
		}
		if err := validatePrivateRegularInfo(info, "replacement SHM sidecar"); err != nil {
			return err
		}
		if err := os.Remove(shmPath); err != nil {
			return fmt.Errorf("remove disposable replacement SHM: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect replacement SHM: %w", err)
	}
	journalPath := path + "-journal"
	if info, err := os.Lstat(journalPath); err == nil {
		if err := validatePrivateRegularInfo(info, "replacement rollback journal"); err != nil {
			return err
		}
		if info.Size() != 0 {
			return errorsf("replacement rollback journal is not empty")
		}
		if err := os.Remove(journalPath); err != nil {
			return fmt.Errorf("remove empty replacement rollback journal: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect replacement rollback journal: %w", err)
	}
	return nil
}

func validateReplacementSidecarTypes(path string) error {
	for _, item := range []struct {
		suffix string
		label  string
	}{{"-wal", "WAL"}, {"-shm", "SHM"}, {"-journal", "rollback journal"}} {
		info, err := os.Lstat(path + item.suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect replacement %s: %w", item.label, err)
		}
		if err := validatePrivateRegularInfo(info, "replacement "+item.label+" sidecar"); err != nil {
			return err
		}
	}
	return nil
}

func prepareUpgradeWithPlan(ctx context.Context, opts UpgradeOptions, plan []migration) (UpgradeResult, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return UpgradeResult{}, err
	}
	source, err := inspectWithPlan(ctx, InspectOptions{Path: opts.SourcePath, MinimumHead: opts.MinimumHead}, plan)
	if err != nil {
		return UpgradeResult{}, err
	}
	if source.Status != InspectionUpgradeRequired {
		return UpgradeResult{}, fmt.Errorf("corestore: database schema is already current at version %d", source.SchemaVersion)
	}

	backupPath, err := destinationPath(opts.BackupPath, "upgrade backup")
	if err != nil {
		return UpgradeResult{}, err
	}
	candidatePath, err := destinationPath(opts.CandidatePath, "upgrade candidate")
	if err != nil {
		return UpgradeResult{}, err
	}
	if source.Path == backupPath || source.Path == candidatePath || backupPath == candidatePath {
		return UpgradeResult{}, errorsf("upgrade source, backup, and candidate paths must differ")
	}
	if filepath.Dir(candidatePath) != filepath.Dir(source.Path) {
		return UpgradeResult{}, errorsf("upgrade candidate must be in the authority directory for atomic publication")
	}

	backup, err := reuseOrCreateUpgradeBackup(ctx, source, backupPath, plan)
	if err != nil {
		return UpgradeResult{}, err
	}
	if err := prepareCandidateDestination(candidatePath, opts.ReplaceCandidate); err != nil {
		return UpgradeResult{}, err
	}
	candidate, err := buildUpgradeCandidate(ctx, source, backup, candidatePath, plan)
	if err != nil {
		return UpgradeResult{}, err
	}
	finalSource, err := inspectWithPlan(ctx, InspectOptions{Path: source.Path, MinimumHead: &source.Head}, plan)
	if err != nil {
		return UpgradeResult{}, err
	}
	if finalSource.SchemaVersion != source.SchemaVersion || finalSource.Head != source.Head {
		return UpgradeResult{}, fmt.Errorf("%w: upgrade source changed while preparing candidate", ErrRollback)
	}
	return UpgradeResult{Source: finalSource, Backup: backup, Candidate: candidate}, nil
}

func reuseOrCreateUpgradeBackup(ctx context.Context, source Inspection, destination string, plan []migration) (BackupInfo, error) {
	info, err := os.Lstat(destination)
	switch {
	case err == nil:
		if err := validatePrivateRegularInfo(info, "upgrade backup"); err != nil {
			return BackupInfo{}, err
		}
		if err := requireNoSQLiteSidecars(destination, "upgrade backup"); err != nil {
			return BackupInfo{}, err
		}
		sourceInfo, statErr := os.Stat(source.Path)
		if statErr != nil {
			return BackupInfo{}, fmt.Errorf("inspect upgrade source: %w", statErr)
		}
		if os.SameFile(sourceInfo, info) {
			return BackupInfo{}, errorsf("upgrade backup must be independent from source")
		}
		backup, verifyErr := verifyBackupWithPlan(ctx, destination, source.Head, source.SchemaVersion, plan)
		if verifyErr != nil {
			return BackupInfo{}, fmt.Errorf("reuse upgrade backup: %w", verifyErr)
		}
		if backup.Head != source.Head || backup.SchemaVersion != source.SchemaVersion {
			return BackupInfo{}, fmt.Errorf("%w: upgrade backup is not at exact source version and head", ErrRollback)
		}
		return backup, nil
	case !errors.Is(err, os.ErrNotExist):
		return BackupInfo{}, fmt.Errorf("inspect upgrade backup: %w", err)
	}
	return createUpgradeBackup(ctx, source, destination, plan)
}

func requireNoSQLiteSidecars(path, label string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect %s%s: %w", label, suffix, err)
		}
		if err := validatePrivateRegularInfo(info, label+suffix); err != nil {
			return err
		}
		return fmt.Errorf("corestore: %s is not standalone: unexpected %s sidecar", label, suffix)
	}
	return nil
}

func createUpgradeBackup(ctx context.Context, source Inspection, destination string, plan []migration) (BackupInfo, error) {
	db, err := sql.Open("sqlite", sqliteDSN(source.Path, defaultBusyTimeout, true))
	if err != nil {
		return BackupInfo{}, fmt.Errorf("open upgrade source: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return BackupInfo{}, fmt.Errorf("open upgrade source: %w", err)
	}
	exact := source.Head
	before, err := inspectDBWithPlan(ctx, db, source.Path, &exact, plan)
	if err != nil {
		return BackupInfo{}, err
	}
	if before.SchemaVersion != source.SchemaVersion || before.Head != source.Head {
		return BackupInfo{}, fmt.Errorf("%w: upgrade source changed before backup", ErrRollback)
	}

	temp, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return BackupInfo{}, fmt.Errorf("create upgrade backup temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer cleanupBackupTemp(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return BackupInfo{}, err
	}
	if err := temp.Close(); err != nil {
		return BackupInfo{}, err
	}
	if err := runOnlineBackupDB(ctx, db, tempPath); err != nil {
		return BackupInfo{}, fmt.Errorf("snapshot upgrade source: %w", err)
	}
	after, err := inspectDBWithPlan(ctx, db, source.Path, &exact, plan)
	if err != nil {
		return BackupInfo{}, err
	}
	if after.SchemaVersion != source.SchemaVersion || after.Head != source.Head {
		return BackupInfo{}, fmt.Errorf("%w: upgrade source changed during backup", ErrRollback)
	}
	if err := normalizeBackupJournal(ctx, tempPath); err != nil {
		return BackupInfo{}, err
	}
	if err := removeBackupSidecars(tempPath); err != nil {
		return BackupInfo{}, err
	}
	if err := syncFile(tempPath); err != nil {
		return BackupInfo{}, err
	}
	backup, err := verifyBackupWithPlan(ctx, tempPath, source.Head, source.SchemaVersion, plan)
	if err != nil {
		return BackupInfo{}, err
	}
	if backup.Head != source.Head {
		return BackupInfo{}, fmt.Errorf("%w: upgrade backup is not at exact source head", ErrRollback)
	}
	if err := os.Link(tempPath, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return BackupInfo{}, errorsf("upgrade backup destination already exists")
		}
		return BackupInfo{}, fmt.Errorf("publish upgrade backup: %w", err)
	}
	if err := syncDir(filepath.Dir(destination)); err != nil {
		return BackupInfo{}, err
	}
	backup.Path = destination
	return backup, nil
}

func prepareCandidateDestination(path string, replace bool) error {
	info, err := os.Lstat(path)
	mainExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect upgrade candidate: %w", err)
	}
	if mainExists {
		if err := validatePrivateRegularInfo(info, "upgrade candidate"); err != nil {
			return err
		}
	}
	sidecars := make([]string, 0, 3)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		sidecar := path + suffix
		info, err := os.Lstat(sidecar)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect upgrade candidate%s: %w", suffix, err)
		}
		if err := validatePrivateRegularInfo(info, "upgrade candidate"+suffix); err != nil {
			return err
		}
		sidecars = append(sidecars, sidecar)
	}
	if !replace && (mainExists || len(sidecars) != 0) {
		return errorsf("upgrade candidate destination already exists")
	}
	if !replace {
		return nil
	}
	for _, sidecar := range sidecars {
		if err := os.Remove(sidecar); err != nil {
			return fmt.Errorf("remove unpublished upgrade candidate sidecar: %w", err)
		}
	}
	if mainExists {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove unpublished upgrade candidate: %w", err)
		}
	}
	if !mainExists && len(sidecars) == 0 {
		return nil
	}
	return syncDir(filepath.Dir(path))
}

func buildUpgradeCandidate(ctx context.Context, source Inspection, backup BackupInfo, destination string, plan []migration) (Inspection, error) {
	temp, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return Inspection{}, fmt.Errorf("create upgrade candidate temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer cleanupBackupTemp(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return Inspection{}, err
	}
	src, err := os.Open(backup.Path)
	if err != nil {
		_ = temp.Close()
		return Inspection{}, fmt.Errorf("open upgrade backup: %w", err)
	}
	_, copyErr := io.Copy(temp, src)
	closeSourceErr := src.Close()
	syncErr := temp.Sync()
	closeTempErr := temp.Close()
	if copyErr != nil {
		return Inspection{}, fmt.Errorf("copy upgrade backup: %w", copyErr)
	}
	if closeSourceErr != nil {
		return Inspection{}, fmt.Errorf("close upgrade backup: %w", closeSourceErr)
	}
	if syncErr != nil {
		return Inspection{}, fmt.Errorf("sync upgrade candidate: %w", syncErr)
	}
	if closeTempErr != nil {
		return Inspection{}, fmt.Errorf("close upgrade candidate: %w", closeTempErr)
	}

	db, err := sql.Open("sqlite", sqliteDSN(tempPath, defaultBusyTimeout, false))
	if err != nil {
		return Inspection{}, fmt.Errorf("open upgrade candidate: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return Inspection{}, fmt.Errorf("open upgrade candidate: %w", err)
	}
	if err := verifyPragmas(ctx, db, defaultBusyTimeout); err != nil {
		_ = db.Close()
		return Inspection{}, err
	}
	before, after, migrationErr := migratePendingAtomically(ctx, db, plan, time.Now().UTC())
	if migrationErr == nil {
		migrationErr = validateUpgradedCandidate(ctx, db, source, before, after, plan)
	}
	closeErr := db.Close()
	if migrationErr != nil {
		return Inspection{}, migrationErr
	}
	if closeErr != nil {
		return Inspection{}, fmt.Errorf("close upgrade candidate: %w", closeErr)
	}
	if err := normalizeBackupJournal(ctx, tempPath); err != nil {
		return Inspection{}, err
	}
	if err := removeBackupSidecars(tempPath); err != nil {
		return Inspection{}, err
	}
	if err := syncFile(tempPath); err != nil {
		return Inspection{}, err
	}
	expectedHead := source.Head
	expectedHead.HeadGeneration++
	verified, err := inspectWithPlan(ctx, InspectOptions{Path: tempPath, MinimumHead: &expectedHead}, plan)
	if err != nil {
		return Inspection{}, err
	}
	if verified.Status != InspectionCurrent || verified.Head != expectedHead {
		return Inspection{}, fmt.Errorf("%w: upgraded candidate has unexpected version or head", ErrRollback)
	}
	if err := os.Link(tempPath, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Inspection{}, errorsf("upgrade candidate destination already exists")
		}
		return Inspection{}, fmt.Errorf("publish upgrade candidate: %w", err)
	}
	if err := syncDir(filepath.Dir(destination)); err != nil {
		return Inspection{}, err
	}
	verified.Path = destination
	return verified, nil
}

func migratePendingAtomically(ctx context.Context, db *sql.DB, plan []migration, now time.Time) (AuthorityHead, AuthorityHead, error) {
	if err := validateMigrationPlan(plan); err != nil {
		return AuthorityHead{}, AuthorityHead{}, err
	}
	var version, appID int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return AuthorityHead{}, AuthorityHead{}, err
	}
	if version < 1 || version >= len(plan) {
		return AuthorityHead{}, AuthorityHead{}, fmt.Errorf("corestore: no supported pending migrations from version %d", version)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&appID); err != nil {
		return AuthorityHead{}, AuthorityHead{}, err
	}
	if appID != applicationID {
		return AuthorityHead{}, AuthorityHead{}, errorsf("application identity mismatch")
	}
	if err := validateSchemaLedgerWithPlan(ctx, db, version, plan); err != nil {
		return AuthorityHead{}, AuthorityHead{}, err
	}
	before, err := readAuthorityHead(ctx, db)
	if err != nil {
		return AuthorityHead{}, AuthorityHead{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return AuthorityHead{}, AuthorityHead{}, fmt.Errorf("begin atomic schema upgrade: %w", err)
	}
	fail := func(err error) (AuthorityHead, AuthorityHead, error) {
		_ = tx.Rollback()
		return AuthorityHead{}, AuthorityHead{}, err
	}
	stamp := formatTime(now)
	for next := version + 1; next <= len(plan); next++ {
		m := plan[next-1]
		for _, stmt := range m.statements {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fail(fmt.Errorf("apply migration %d: %w", next, err))
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES (?,?,?,?)`, m.version, m.name, migrationChecksum(m), stamp); err != nil {
			return fail(fmt.Errorf("record migration %d: %w", next, err))
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA application_id = %d`, applicationID)); err != nil {
		return fail(fmt.Errorf("stamp application identity: %w", err))
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, len(plan))); err != nil {
		return fail(fmt.Errorf("stamp schema version: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE store_meta SET head_generation=head_generation+1, updated_at=? WHERE singleton=1`, stamp); err != nil {
		return fail(fmt.Errorf("advance upgrade authority head: %w", err))
	}
	after, err := readAuthorityHead(ctx, tx)
	if err != nil {
		return fail(fmt.Errorf("read upgraded authority head: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return AuthorityHead{}, AuthorityHead{}, fmt.Errorf("commit atomic schema upgrade: %w", err)
	}
	return before, after, nil
}

func validateUpgradedCandidate(ctx context.Context, db *sql.DB, source Inspection, before, after AuthorityHead, plan []migration) error {
	if before != source.Head {
		return fmt.Errorf("%w: candidate did not start at exact source head", ErrRollback)
	}
	want := before
	want.HeadGeneration++
	if after != want {
		return fmt.Errorf("%w: schema upgrade did not advance only head generation exactly once", ErrRollback)
	}
	if err := validateSchemaLedgerWithPlan(ctx, db, len(plan), plan); err != nil {
		return fmt.Errorf("validate upgraded schema: %w", err)
	}
	report, err := checkIntegrityDB(ctx, db)
	if err != nil {
		return fmt.Errorf("validate upgraded integrity: %w", err)
	}
	if !report.OK() {
		return integrityFailure(report)
	}
	got, err := readAuthorityHead(ctx, db)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: upgraded candidate head changed during validation", ErrRollback)
	}
	return nil
}

func existingRegularPath(path, label string) (string, os.FileInfo, error) {
	if path == "" {
		return "", nil, fmt.Errorf("corestore: %s path is required", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve %s path: %w", label, err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", nil, fmt.Errorf("inspect %s path: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("corestore: %s path must be a regular file, not a symbolic link", label)
	}
	if err := validatePrivateRegularInfo(info, label); err != nil {
		return "", nil, err
	}
	if err := ensurePrivateParent(filepath.Dir(abs)); err != nil {
		return "", nil, err
	}
	return abs, info, nil
}

func validatePrivateRegularInfo(info os.FileInfo, label string) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("corestore: %s must be a regular file, not a symbolic link", label)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("corestore: %s must not be group or world accessible", label)
	}
	return nil
}

func destinationPath(path, label string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("corestore: %s path is required", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s path: %w", label, err)
	}
	if err := ensurePrivateParent(filepath.Dir(abs)); err != nil {
		return "", err
	}
	return abs, nil
}
