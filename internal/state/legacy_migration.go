package state

import (
	"errors"
	"os"
	"time"

	"smartgate/internal/smartnode"
)

const LegacyBindingMigrationSchemaVersion = 1

// LegacyBindingMigrationReport records how pre-SmartNode (V2) pool bindings
// and selector choices were translated on the first V3 start. It is durable so
// operators can see which bindings were dropped long after the boot log has
// rotated.
type LegacyBindingMigrationReport struct {
	SchemaVersion int                       `json:"schema_version"`
	MigratedAt    time.Time                 `json:"migrated_at"`
	Resolved      []LegacyBindingResolution `json:"resolved,omitempty"`
	Unresolved    []LegacyBindingReference  `json:"unresolved,omitempty"`
	Warnings      []string                  `json:"warnings,omitempty"`
}

type LegacyBindingResolution struct {
	LegacyID  string           `json:"legacy_id"`
	SmartNode string           `json:"smart_node"`
	Core      smartnode.CoreID `json:"core,omitempty"`
}

type LegacyBindingReference struct {
	Scope    string `json:"scope"`
	Pool     string `json:"pool,omitempty"`
	LegacyID string `json:"legacy_id"`
}

func (l Layout) LegacyMigrationReportPath() string {
	return l.SpecPath("legacy-migration")
}

func (s *Store) LoadLegacyMigrationReport() (LegacyBindingMigrationReport, bool, error) {
	var report LegacyBindingMigrationReport
	err := loadJSON(s.layout.LegacyMigrationReportPath(), &report)
	if errors.Is(err, os.ErrNotExist) {
		return LegacyBindingMigrationReport{}, false, nil
	}
	if err != nil {
		return LegacyBindingMigrationReport{}, false, err
	}
	return report, true, nil
}

func (s *Store) SaveLegacyMigrationReport(report LegacyBindingMigrationReport) error {
	if report.SchemaVersion == 0 {
		report.SchemaVersion = LegacyBindingMigrationSchemaVersion
	}
	return saveJSONDurable(s.layout.LegacyMigrationReportPath(), report, 0o600)
}
