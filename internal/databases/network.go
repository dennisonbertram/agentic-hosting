package databases

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// EnginePort is the port the database process listens on inside its container.
func EnginePort(dbType string) int {
	switch dbType {
	case "postgres":
		return 5432
	case "redis":
		return 6379
	default:
		return 0
	}
}

func buildConnectionString(dbType, dbID, username, password, dbName string) string {
	host := docker.DatabaseAlias(dbID)
	switch dbType {
	case "postgres":
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", username, password, host, EnginePort(dbType), dbName)
	case "redis":
		return fmt.Sprintf("redis://:%s@%s:%d/0", password, host, EnginePort(dbType))
	default:
		return ""
	}
}

// exposeEngineAddress rewrites API host/port to the overlay DNS name and
// container port. The databases.port column remains the host-loopback probe port.
func exposeEngineAddress(d *Database) {
	if d == nil {
		return
	}
	if d.ID != "" {
		d.Host = docker.DatabaseAlias(d.ID)
	}
	if p := EnginePort(d.Type); p > 0 {
		d.Port = p
	}
}

func (m *Manager) removeDatabaseNetwork(ctx context.Context, tenantID, dbID string) {
	name := docker.DatabaseNetworkName(tenantID, dbID)
	info, err := m.docker.NetworkInspect(ctx, name)
	if err == nil {
		for _, cname := range info.ContainerNames {
			if cname == "" {
				continue
			}
			if disconnErr := m.docker.NetworkDisconnect(ctx, name, cname); disconnErr != nil {
				log.Printf("databases: disconnect %s from %s: %v", cname, name, disconnErr)
			}
		}
	}
	if err := m.docker.RemoveNetwork(ctx, name); err != nil {
		log.Printf("databases: remove network %s: %v", name, err)
	}
}

// migrateLoopbackURLs rewrites stored 127.0.0.1 connection strings (and matching
// service_env copies) to the per-database DNS alias and container port.
func (m *Manager) migrateLoopbackURLs() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, type, status, COALESCE(host, ''), port, COALESCE(connection_string_encrypted, '')
		 FROM databases WHERE port IS NOT NULL`)
	if err != nil {
		log.Printf("databases: migrate loopback URLs query failed: %v", err)
		return
	}
	defer rows.Close()

	type row struct {
		id, tenantID, typ, status, host, connEnc string
		port                                     int
	}
	var dbs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.tenantID, &r.typ, &r.status, &r.host, &r.port, &r.connEnc); err != nil {
			log.Printf("databases: migrate loopback URLs scan failed: %v", err)
			continue
		}
		dbs = append(dbs, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("databases: migrate loopback URLs rows: %v", err)
		return
	}

	rewritten := 0
	envRewritten := 0
	for _, r := range dbs {
		oldAddr := "127.0.0.1:" + strconv.Itoa(r.port)
		newAddr := docker.DatabaseAlias(r.id) + ":" + strconv.Itoa(EnginePort(r.typ))
		if r.host == "127.0.0.1" {
			if _, err := m.db.ExecContext(ctx,
				`UPDATE databases SET host = ?, updated_at = ? WHERE id = ?`,
				docker.DatabaseAlias(r.id), time.Now().Unix(), r.id,
			); err != nil {
				log.Printf("databases: migrate host for %s: %v", r.id, err)
			}
		}
		if r.connEnc != "" {
			plain, err := crypto.Decrypt(r.connEnc, m.masterKey)
			if err == nil {
				next, changed := rewriteLoopbackAddress(string(plain), oldAddr, newAddr)
				if changed {
					enc, encErr := crypto.Encrypt([]byte(next), m.masterKey)
					if encErr != nil {
						log.Printf("databases: migrate re-encrypt connection string for %s: %v", r.id, encErr)
					} else if _, err := m.db.ExecContext(ctx,
						`UPDATE databases SET connection_string_encrypted = ?, updated_at = ? WHERE id = ?`,
						enc, time.Now().Unix(), r.id,
					); err != nil {
						log.Printf("databases: migrate connection string for %s: %v", r.id, err)
					} else {
						rewritten++
					}
				}
			}
		}
		// A failed row's host port is free for reuse (the unique port index
		// excludes failed rows), so a service_env value on that port may point at
		// a live database. Only live rows own their port.
		if r.status == "failed" {
			continue
		}
		n, err := m.rewriteServiceEnvLoopback(ctx, r.tenantID, oldAddr, newAddr)
		if err != nil {
			log.Printf("databases: migrate service_env for %s: %v", r.id, err)
			continue
		}
		envRewritten += n
	}
	if rewritten > 0 || envRewritten > 0 {
		log.Printf("databases: migrated %d connection strings and %d service_env values off 127.0.0.1", rewritten, envRewritten)
	}
}

// rewriteLoopbackAddress replaces each occurrence of oldAddr ("host:port") in
// value with newAddr. An occurrence followed by another digit names a different
// port (127.0.0.1:5432 inside 127.0.0.1:54321) and is left alone.
func rewriteLoopbackAddress(value, oldAddr, newAddr string) (string, bool) {
	if oldAddr == "" {
		return value, false
	}
	var b strings.Builder
	changed := false
	rest := value
	for {
		i := strings.Index(rest, oldAddr)
		if i < 0 {
			break
		}
		end := i + len(oldAddr)
		b.WriteString(rest[:i])
		if end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			b.WriteString(oldAddr)
		} else {
			b.WriteString(newAddr)
			changed = true
		}
		rest = rest[end:]
	}
	if !changed {
		return value, false
	}
	b.WriteString(rest)
	return b.String(), true
}

func (m *Manager) rewriteServiceEnvLoopback(ctx context.Context, tenantID, oldAddr, newAddr string) (int, error) {
	if oldAddr == "" || oldAddr == newAddr {
		return 0, nil
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT se.service_id, se.key, se.value_encrypted
		 FROM service_env se
		 JOIN services s ON s.id = se.service_id
		 WHERE s.tenant_id = ?`, tenantID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type envRow struct {
		serviceID, key, enc string
	}
	var envs []envRow
	for rows.Next() {
		var e envRow
		if err := rows.Scan(&e.serviceID, &e.key, &e.enc); err != nil {
			return 0, err
		}
		envs = append(envs, e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	updated := 0
	now := time.Now().Unix()
	for _, e := range envs {
		plain, err := crypto.Decrypt(e.enc, m.masterKey)
		if err != nil {
			continue
		}
		next, changed := rewriteLoopbackAddress(string(plain), oldAddr, newAddr)
		if !changed {
			continue
		}
		enc, err := crypto.Encrypt([]byte(next), m.masterKey)
		if err != nil {
			return updated, err
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE service_env SET value_encrypted = ?, updated_at = ? WHERE service_id = ? AND key = ?`,
			enc, now, e.serviceID, e.key,
		); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}
