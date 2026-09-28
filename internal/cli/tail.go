package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
)

// tail prints a worker's rpc log (the driver's file for its latest run) readably; -f follows it,
// onto the next run when the worker is resumed, until Ctrl-C. --json prints the raw records.
func (e *env) tail(args []string) error {
	fs := e.flags("tail")
	var a core.WorkerLogArgs
	n := fs.Int("n", 20, "readable lines to show")
	follow := fs.Bool("f", false, "follow until Ctrl-C")
	fs.StringVar(&a.Team, "team", "", "team, when the name is in more than one")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: tail <worker> [-n N] [-f] [--team T]", errUsage)
	}
	a.Worker = pos[0]
	c, err := e.connect()
	if err != nil {
		return err
	}
	defer c.Close()
	var r proto.TailResult
	if _, err := c.CallInto(proto.VerbTail, a, &r); err != nil {
		return err
	}
	show := func(rec []byte) {
		if e.json {
			fmt.Fprintln(e.stdout, string(rec))
		} else if l, ok := tailLine(rec); ok {
			fmt.Fprintln(e.stdout, l)
		}
	}
	recs, off, err := readRecords(r.Path, 0)
	if err != nil {
		return err
	}
	if !e.json {
		recs = readable(recs)
	}
	if len(recs) > *n {
		recs = recs[len(recs)-*n:]
	}
	for _, rec := range recs {
		show(rec)
	}
	if !*follow {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for i := 1; ; i++ {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		recs, off, err = readRecords(r.Path, off)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			show(rec)
		}
		if i%5 == 0 { // a resumed worker writes a new run's log
			var next proto.TailResult
			if _, err := c.CallInto(proto.VerbTail, a, &next); err != nil {
				return err
			}
			if next.RunID != r.RunID {
				r, off = next, 0
				fmt.Fprintf(e.stdout, "-- run %s\n", r.RunID)
			}
		}
	}
}

// readRecords returns the complete lines of path from byte offset off, and the offset after
// them. A missing file (the run has not written yet) is empty.
func readRecords(path string, off int64) ([][]byte, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, off, nil
	}
	if err != nil {
		return nil, off, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, off, err
	}
	var out [][]byte
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil { // a partial last line is read again next time
			return out, off, nil
		}
		off += int64(len(line))
		if line = bytes.TrimRight(line, "\r\n"); len(line) > 0 {
			out = append(out, line)
		}
	}
}

// readable keeps the records tailLine shows.
func readable(recs [][]byte) [][]byte {
	var out [][]byte
	for _, r := range recs {
		if _, ok := tailLine(r); ok {
			out = append(out, r)
		}
	}
	return out
}

// tailLine is one pi rpc record (docs/rpc.md of pi) as a line, or false for records that only
// frame others (starts, settles, UI).
// toolArgs is a tool call's arguments as a person reads them: bash its command; read, edit and
// write their path; another tool its one argument, or key=value pairs (strings short, in key
// order). Not an object: as sent.
func toolArgs(name string, raw json.RawMessage) string {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return string(raw)
	}
	key := map[string]string{"bash": "command", "read": "path", "edit": "path", "write": "path"}[name]
	if v, ok := args[key].(string); ok {
		return v
	}
	value := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	if len(args) == 1 {
		for _, v := range args {
			return value(v)
		}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		v := value(args[k])
		if r := []rune(v); len(r) > 40 {
			v = string(r[:39]) + "…"
		}
		parts[i] = k + "=" + v
	}
	return strings.Join(parts, " ")
}

func tailLine(rec []byte) (string, bool) {
	var r struct {
		Type    string `json:"type"`
		Message struct {
			Role         string          `json:"role"`
			Content      json.RawMessage `json:"content"`
			StopReason   string          `json:"stopReason"`
			ErrorMessage string          `json:"errorMessage"`
		} `json:"message"`
		ToolName     string                            `json:"toolName"`
		Args         json.RawMessage                   `json:"args"`
		Result       struct{ Content json.RawMessage } `json:"result"`
		IsError      bool                              `json:"isError"`
		Attempt      int                               `json:"attempt"`
		MaxAttempts  int                               `json:"maxAttempts"`
		ErrorMessage string                            `json:"errorMessage"`
		Reason       string                            `json:"reason"`
		Command      string                            `json:"command"`
		Success      *bool                             `json:"success"`
		Error        string                            `json:"error"`
	}
	if json.Unmarshal(rec, &r) != nil {
		return "", false
	}
	switch r.Type {
	case "message_end":
		m := r.Message
		switch {
		case m.StopReason == "error" || m.ErrorMessage != "":
			return "! " + m.Role + " error: " + clip(m.ErrorMessage), true
		case m.Role == "user" || m.Role == "assistant":
			if t := text(m.Content); t != "" {
				return m.Role + ": " + clip(t), true
			}
		}
		return "", false // tool calls and results show as tool lines
	case "tool_execution_start":
		return "> " + r.ToolName + " " + clip(toolArgs(r.ToolName, r.Args)), true
	case "tool_execution_end":
		status := "ok"
		if r.IsError {
			status = "error"
		}
		return "< " + r.ToolName + " " + status + ": " + clip(text(r.Result.Content)), true
	case "agent_end":
		return "-- agent end", true
	case "auto_retry_start":
		return fmt.Sprintf("! retry %d/%d: %s", r.Attempt, r.MaxAttempts, clip(r.ErrorMessage)), true
	case "compaction_start":
		return "-- compaction (" + r.Reason + ")", true
	case "response":
		if r.Success != nil && !*r.Success {
			return "! " + r.Command + " failed: " + clip(r.Error), true
		}
	}
	return "", false
}

// text joins the text blocks of a message content (a string or a list of blocks).
func text(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct{ Type, Text string }
	json.Unmarshal(content, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}

// control matches terminal escape sequences (CSI such as colors, OSC, other ESC sequences) and
// the remaining control characters: tool output in worker logs carries them (e.g. a server's
// colored warning), and printed raw they would restyle or move the operator's terminal.
var control = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-_]?|[\x00-\x08\x0b\x0c\x0e-\x1f\x7f\x{80}-\x{9f}]`)

// clip is s on one line without control characters, cut to 200 runes.
func clip(s string) string {
	s = strings.Join(strings.Fields(control.ReplaceAllString(s, "")), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
