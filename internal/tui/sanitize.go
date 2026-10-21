package tui

import "github.com/SuSonicTH/cpt/internal/app"

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
