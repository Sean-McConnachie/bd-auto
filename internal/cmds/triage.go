package cmds

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"bd-auto/internal/drain"
)

// Triage implements `bd-auto triage`: decide what becomes of the work a run's
// workers found.
//
// Under `discovered_work: triage` — the default — a wave barrier files nothing.
// It stages findings in .beads/auto/triage.json and this is what turns one into
// an issue, folds it into an issue that already exists, or discards it.
//
// It is scriptable rather than only interactive, because the two things that
// have to be able to drive it are a human at a terminal and a smoke test.
func Triage(args []string) error {
	fs := flag.NewFlagSet("triage", flag.ContinueOnError)
	list := fs.Bool("list", false, "show what is waiting (the default)")
	all := fs.Bool("all", false, "with --list, include what has already been decided")
	accept := fs.String("accept", "", "file this discovery as an issue; takes a key or a unique prefix of one")
	into := fs.String("into", "", "with --accept, fold it into this existing issue as a note instead of filing anything")
	discard := fs.String("discard", "", "record that this discovery is not work")
	reason := fs.String("reason", "", "with --discard, why")
	acceptAll := fs.Bool("accept-all", false, "file every pending discovery")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := NewCtx()
	if err != nil {
		return err
	}
	t, err := drain.LoadTriage(c.RepoRoot)
	if err != nil {
		return err
	}
	eng := &drain.Engine{RepoRoot: c.RepoRoot, Cfg: c.Cfg, BD: c.BD}

	switch {
	case *accept != "" && *into != "":
		return decide(eng.Merge(t, *accept, *into))(*asJSON)
	case *accept != "":
		return decide(eng.Accept(t, *accept))(*asJSON)
	case *discard != "":
		return decide(eng.Discard(t, *discard, *reason))(*asJSON)
	case *into != "":
		return errors.New("--into folds a discovery into an issue, and needs --accept to say which discovery")
	case *acceptAll:
		return acceptEvery(eng, t, *asJSON)
	default:
		_ = *list
		return listStaged(c.RepoRoot, t, *all, *asJSON)
	}
}

// decide reports one outcome, in whichever form was asked for.
func decide(d drain.TriageDecision, err error) func(bool) error {
	return func(asJSON bool) error {
		if err != nil {
			// A decision that partly landed reports what landed and then fails,
			// because a retry would file the issue a second time.
			if d.Outcome != "" {
				_ = report(d, asJSON)
			}
			return err
		}
		return report(d, asJSON)
	}
}

func report(d drain.TriageDecision, asJSON bool) error {
	if asJSON {
		return emitJSON(d)
	}
	switch d.Outcome {
	case "filed":
		fmt.Printf("filed %s: %s\n", d.FiledAs, d.Title)
	case "merged":
		fmt.Printf("folded into %s: %s\n", d.MergedInto, d.Title)
	case "discarded":
		fmt.Printf("discarded: %s (%s)\n", d.Title, d.Reason)
	}
	return nil
}

func acceptEvery(eng *drain.Engine, t *drain.Triage, asJSON bool) error {
	pending := t.Pending()
	if len(pending) == 0 {
		return report(drain.TriageDecision{}, asJSON)
	}
	var out []drain.TriageDecision
	for _, s := range pending {
		d, err := eng.Accept(t, s.Key)
		if err != nil {
			// Everything filed so far is reported before the failure, so a
			// human can see what already exists rather than re-running blind.
			if len(out) > 0 && !asJSON {
				for _, o := range out {
					_ = report(o, false)
				}
			}
			return err
		}
		out = append(out, d)
	}
	if asJSON {
		return emitJSON(out)
	}
	for _, o := range out {
		_ = report(o, false)
	}
	return nil
}

func listStaged(repoRoot string, t *drain.Triage, all, asJSON bool) error {
	rows := t.Pending()
	if all {
		rows = append([]drain.Staged(nil), t.Staged...)
		sort.Slice(rows, func(i, j int) bool { return rows[i].Found.Before(rows[j].Found) })
	}
	if asJSON {
		if rows == nil {
			rows = []drain.Staged{}
		}
		return emitJSON(rows)
	}
	if len(rows) == 0 {
		fmt.Println("nothing staged for triage.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tFROM\tLOOKS LIKE\tTITLE")
	for _, s := range rows {
		like := ""
		if s.Resembles != "" {
			like = fmt.Sprintf("%s (%.2f)", s.Resembles, s.Score)
		}
		state := ""
		if !s.Pending() {
			state = " [" + s.Outcome + "]"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s%s\n", shortKey(s.Key), s.From, like, s.Title, state)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d waiting. `bd-auto triage --accept <key>` files one, "+
		"`--accept <key> --into <issue>` folds it into an issue that already exists, "+
		"`--discard <key> --reason \"...\"` says it is not work.\n", len(t.Pending()))
	return nil
}

// shortKey is the prefix of a key that is enough to type. The key is a whole
// issue title, and the commands accept any unique prefix.
func shortKey(key string) string {
	const width = 28
	if len(key) <= width {
		return key
	}
	return strings.TrimSpace(key[:width]) + "…"
}
