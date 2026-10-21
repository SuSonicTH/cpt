// Package out renders CLI output as text, coloured text or JSON.
package out

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Envelope is the JSON shape emitted by every command in --json mode.
type Envelope struct {
	OK    bool     `json:"ok"`
	Data  any      `json:"data,omitempty"`
	Error *Payload `json:"error,omitempty"`
}

// Payload describes a failure.
type Payload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Printer writes command output.
type Printer struct {
	Out   io.Writer
	Err   io.Writer
	JSON  bool
	Color bool
	Quiet bool
}

// New builds a printer.
func New(stdout, stderr io.Writer, jsonMode, color, quiet bool) *Printer {
	return &Printer{Out: stdout, Err: stderr, JSON: jsonMode, Color: color, Quiet: quiet}
}

// Line writes a plain line to stdout.
func (p *Printer) Line(format string, args ...any) {
	fmt.Fprintf(p.Out, format+"\n", args...)
}

// Infof writes informational output to stderr so stdout stays parseable.
func (p *Printer) Infof(format string, args ...any) {
	if p.Quiet || p.JSON {
		return
	}
	fmt.Fprintf(p.Err, format+"\n", args...)
}

// Warnf writes a warning to stderr.
func (p *Printer) Warnf(format string, args ...any) {
	if p.Quiet {
		return
	}
	fmt.Fprintf(p.Err, "warning: "+format+"\n", args...)
}

// Errorf writes an error to stderr.
func (p *Printer) Errorf(format string, args ...any) {
	fmt.Fprintf(p.Err, "cpt: "+format+"\n", args...)
}

// Table renders aligned columns.
func (p *Printer) Table(headers []string, rows [][]string) {
	tw := tabwriter.NewWriter(p.Out, 2, 4, 2, ' ', 0)
	if len(headers) > 0 {
		fmt.Fprintln(tw, strings.Join(headers, "\t"))
	}
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	_ = tw.Flush()
}

// JSONData emits a success envelope.
func (p *Printer) JSONData(v any) error {
	return p.jsonEnvelope(Envelope{OK: true, Data: v})
}

// JSONError emits a failure envelope.
func (p *Printer) JSONError(code int, err error) error {
	return p.jsonEnvelope(Envelope{OK: false, Error: &Payload{Code: code, Message: err.Error()}})
}

func (p *Printer) jsonEnvelope(env Envelope) error {
	enc := json.NewEncoder(p.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}

// Symbol prefixes status text when colour is enabled.
func (p *Printer) Symbol(kind, s string) string {
	if !p.Color {
		return s
	}
	code := map[string]string{
		"ok":    "32",
		"warn":  "33",
		"error": "31",
		"dim":   "2",
		"bold":  "1",
	}[kind]
	if code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
