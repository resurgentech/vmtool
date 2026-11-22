package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TemplateRow is one template of a host's last report. Created is nil when
// the host did not know it; SourceImage is empty on a saved template. A host
// may hold two rows of one name in one pool (a stale image template beside
// the current one), so the table has no key.
type TemplateRow struct {
	Host        string
	Name        string
	Kind        string
	Pool        string
	SourceImage string
	SizeBytes   int64
	Created     *time.Time
}

// ApplyReport applies one host's full VM list, as ApplyReportWithTemplates
// does for a report that carried no templates: the host's stored templates and
// its templates_reported flag are left as they were.
func (d *DB) ApplyReport(ctx context.Context, host string, generatedAt time.Time, version string, vms []VMRow) (ReportCounts, error) {
	return d.ApplyReportWithTemplates(ctx, host, generatedAt, version, vms, nil)
}

// replaceTemplates swaps the host's stored templates for rows and marks the
// host as having reported templates. The caller has already written the
// host's row.
func replaceTemplates(ctx context.Context, tx *sql.Tx, host string, rows []TemplateRow) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM templates WHERE host=?`, host); err != nil {
		return err
	}
	for _, r := range rows {
		created := ""
		if r.Created != nil {
			created = r.Created.UTC().Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO templates(host, name, kind, pool, source_image, size_bytes, created) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			host, r.Name, r.Kind, r.Pool, r.SourceImage, r.SizeBytes, created); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE hosts SET templates_reported=1 WHERE name=?`, host)
	return err
}

// listTemplates returns the templates of host (all hosts when empty), by host,
// name, pool and then the order they were reported in.
func (d *DB) listTemplates(ctx context.Context, host string) ([]TemplateRow, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT host, name, kind, pool, source_image, size_bytes, created FROM templates
		 WHERE (?='' OR host=?) ORDER BY host, name, pool, rowid`, host, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemplateRow
	for rows.Next() {
		var r TemplateRow
		var created string
		if err := rows.Scan(&r.Host, &r.Name, &r.Kind, &r.Pool, &r.SourceImage, &r.SizeBytes, &created); err != nil {
			return nil, err
		}
		if created != "" {
			c := parseTime(created)
			r.Created = &c
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func checkTemplates(rows []TemplateRow) error {
	for _, r := range rows {
		if r.Name == "" || r.Kind == "" {
			return errors.New("apply report: a template needs a name and a kind")
		}
	}
	return nil
}
