package tui

import (
	"github.com/SuSonicTH/cpt/internal/app"
	"github.com/SuSonicTH/cpt/internal/sshtarget"
)

// List, log and snippet text is remote input. Strip terminal control sequences
// before it reaches the renderer so a list cannot spoof the approval UI.

func sanitizeRows(rows []app.Row) {
	for i := range rows {
		r := &rows[i]
		r.Name = sanitize(r.Name, false)
		r.List = sanitize(r.List, false)
		r.Version = sanitize(r.Version, false)
		r.Latest = sanitize(r.Latest, false)
		r.Status = sanitize(r.Status, false)
		r.Description = sanitize(r.Description, false)
		r.Tags = sanitize(r.Tags, false)
		r.Homepage = sanitize(r.Homepage, false)
		r.License = sanitize(r.License, false)
		r.Asset = sanitize(r.Asset, false)
		r.Checksum = sanitize(r.Checksum, false)
		r.ChecksumSource = sanitize(r.ChecksumSource, false)
		r.SHA256 = sanitize(r.SHA256, false)
		r.Error = sanitize(r.Error, false)
		for j := range r.Bins {
			r.Bins[j] = sanitize(r.Bins[j], false)
		}
	}
}

func sanitizeLists(lists []app.ListInfo) []app.ListInfo {
	out := make([]app.ListInfo, len(lists))
	for i, li := range lists {
		li.Name = sanitize(li.Name, false)
		li.Origin = sanitize(li.Origin, false)
		out[i] = li
	}
	return out
}

func sanitizeBundles(bundles []app.BundleInfo) []app.BundleInfo {
	out := make([]app.BundleInfo, len(bundles))
	for i, b := range bundles {
		b.Name = sanitize(b.Name, false)
		b.Path = sanitize(b.Path, false)
		b.Manifest = sanitize(b.Manifest, false)
		b.Target = sanitize(b.Target, false)
		out[i] = b
	}
	return out
}

func sanitizeShells(shells []app.ShellInfo) []app.ShellInfo {
	out := make([]app.ShellInfo, len(shells))
	for i, info := range shells {
		info.Package = sanitize(info.Package, false)
		info.LiveVersion = sanitize(info.LiveVersion, false)
		info.PendingVersion = sanitize(info.PendingVersion, false)
		info.LastError = sanitize(info.LastError, false)
		out[i] = info
	}
	return out
}

func sanitizeSSHHosts(hosts []sshtarget.ConfigHost) []sshtarget.ConfigHost {
	out := make([]sshtarget.ConfigHost, len(hosts))
	for i, h := range hosts {
		h.Alias = sanitize(h.Alias, false)
		out[i] = h
	}
	return out
}

func sanitizeSSHTargets(targets []app.SSHTargetInfo) []app.SSHTargetInfo {
	out := make([]app.SSHTargetInfo, len(targets))
	for i, t := range targets {
		t.Name = sanitize(t.Name, false)
		t.Bundle = sanitize(t.Bundle, false)
		t.SSH.Source = sanitize(t.SSH.Source, false)
		t.SSH.Host = sanitize(t.SSH.Host, false)
		t.SSH.User = sanitize(t.SSH.User, false)
		t.SSH.IdentityFile = sanitize(t.SSH.IdentityFile, false)
		t.SSH.ProxyJump = sanitize(t.SSH.ProxyJump, false)
		t.RemoteRoot = sanitize(t.RemoteRoot, false)
		t.RemoteRootDisplay = sanitize(t.RemoteRootDisplay, false)
		t.Destination = sanitize(t.Destination, false)
		t.BundlePath = sanitize(t.BundlePath, false)
		t.BundleTarget = sanitize(t.BundleTarget, false)
		t.ResolveError = sanitize(t.ResolveError, false)
		if t.Resolved != nil {
			resolved := *t.Resolved
			resolved.Alias = sanitize(resolved.Alias, false)
			resolved.HostName = sanitize(resolved.HostName, false)
			resolved.User = sanitize(resolved.User, false)
			resolved.IdentityFile = sanitize(resolved.IdentityFile, false)
			resolved.ProxyJump = sanitize(resolved.ProxyJump, false)
			t.Resolved = &resolved
		}
		out[i] = t
	}
	return out
}
