package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// ah runs as an unprivileged user and cannot read iptables itself. The
// systemd drop-in from docs/runbooks/sandboxes-hardening.md runs, as root and
// right before ah starts:
//
//	sandbox-egress-firewall.sh install ...
//	sandbox-egress-firewall.sh snapshot /run/ah-sandbox-egress/firewall.rules
//
// and ah checks the rules in that snapshot. Without a snapshot (ah running as
// root, e.g. a dev host) it runs iptables -S directly.

// defaultFirewallSnapshotPath is where the drop-in writes the snapshot.
const defaultFirewallSnapshotPath = "/run/ah-sandbox-egress/firewall.rules"

// firewallSnapshotMaxAge bounds how old a snapshot may be: it is written
// just before ah starts, so an old one means the drop-in no longer runs.
const firewallSnapshotMaxAge = 10 * time.Minute

// firewallSnapshotRunner returns a commandRunner answering "BIN -w -S CHAIN"
// from the snapshot at path. The file must be a regular file owned by
// ownerUID (root in production), not writable by group or others, and fresh;
// otherwise ah cannot trust it.
func firewallSnapshotRunner(path string, now time.Time, ownerUID uint32) (commandRunner, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("firewall snapshot %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("firewall snapshot %s is writable by group or others (mode %v)", path, fi.Mode().Perm())
	}
	if uid, ok := fileOwnerUID(fi); ok && uid != ownerUID {
		return nil, fmt.Errorf("firewall snapshot %s is owned by uid %d, want %d (root)", path, uid, ownerUID)
	}
	if age := now.Sub(fi.ModTime()); age > firewallSnapshotMaxAge {
		return nil, fmt.Errorf("firewall snapshot %s is %s old; it must be written right before ah starts (systemd drop-in, see docs/runbooks/sandboxes-hardening.md)", path, age.Round(time.Second))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sections := parseFirewallSnapshot(string(data))
	return func(_ context.Context, name string, args ...string) (string, error) {
		chain := ""
		if len(args) > 0 {
			chain = args[len(args)-1]
		}
		out, ok := sections[name+" "+chain]
		if !ok {
			return "", fmt.Errorf("not in firewall snapshot %s", path)
		}
		return out, nil
	}, nil
}

// parseFirewallSnapshot splits a snapshot into "BIN CHAIN" -> iptables -S
// output. A section marked "#!unreadable" is left out.
func parseFirewallSnapshot(data string) map[string]string {
	sections := map[string]string{}
	key := ""
	var b strings.Builder
	flush := func() {
		if key != "" && !strings.Contains(b.String(), "#!unreadable") {
			sections[key] = b.String()
		}
		b.Reset()
	}
	for _, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, "### ") {
			flush()
			key = strings.TrimSpace(strings.TrimPrefix(line, "### "))
			continue
		}
		if key != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	flush()
	return sections
}

// egressFirewallRunner picks how the firewall check reads rules: the
// snapshot when one exists, iptables otherwise.
func egressFirewallRunner(path string, now time.Time) (commandRunner, string, error) {
	run, err := firewallSnapshotRunner(path, now, 0)
	if err == nil {
		return run, "snapshot " + path, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return runHostCommand, "iptables", nil
	}
	return nil, "", err
}
