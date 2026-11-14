// llmgwctl manages the Kleinia model gateway: sources, keys and usage.
// It talks to the same SQLite file as the daemon (WAL allows concurrent
// access); the daemon picks up source changes within 30s or on SIGHUP,
// key changes are visible immediately.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"kleinia.io/llmgw/internal/store"
)

func main() {
	db := flag.String("db", "var/state.db", "SQLite state database path")
	flag.Parse()
	if flag.NArg() < 1 {
		usage()
	}
	st, err := store.Open(*db)
	must(err)
	defer st.Close()

	cmd := flag.Arg(0)
	args := flag.Args()[1:]
	switch cmd {
	case "source":
		must(sourceCmd(st, args))
	case "key":
		must(keyCmd(st, args))
	case "usage":
		must(usageCmd(st, args))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `llmgwctl — manage the Kleinia model gateway

Usage:
  llmgwctl source add NAME --type openai|openai-responses|anthropic --base URL
              (--key K | --key-file F) [--priority N] [--weight N]
              [--model alias=upstream]...          # bare --model x maps x→x
  llmgwctl source list
  llmgwctl source remove NAME
  llmgwctl key add NAME [--models a,b|'*'] [--rpm N] [--tpm N] [--daily-tokens N]
  llmgwctl key list
  llmgwctl key revoke NAME
  llmgwctl usage [--since 24h]      # aggregates by key/model/source
  llmgwctl usage recent [--limit N] # last raw rows`)
	os.Exit(2)
}

func sourceCmd(st *store.Store, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("source needs a subcommand")
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: source add NAME --type ... --base URL (--key K|--key-file F) [--model alias=up]...")
		}
		name := args[1]
		fs := flag.NewFlagSet("source add", flag.ExitOnError)
		typ := fs.String("type", "openai", "source type: openai|anthropic")
		base := fs.String("base", "", "upstream base URL (required, no default)")
		key := fs.String("key", "", "API key (prefer --key-file)")
		keyFile := fs.String("key-file", "", "file containing the API key")
		priority := fs.Int("priority", 0, "priority tier (higher wins)")
		weight := fs.Int("weight", 0, "weight within the tier")
		var models multiFlag
		fs.Var(&models, "model", "model mapping alias=upstream (repeatable)")
		must(fs.Parse(args[2:]))
		if fs.NArg() != 0 || *base == "" {
			return fmt.Errorf("usage: source add NAME --type ... --base URL (--key K|--key-file F) [--model alias=up]...")
		}
		apiKey := *key
		if apiKey == "" && *keyFile != "" {
			b, err := os.ReadFile(*keyFile)
			if err != nil {
				return err
			}
			apiKey = strings.TrimSpace(string(b))
		}
		if apiKey == "" {
			return fmt.Errorf("no API key given (--key or --key-file)")
		}
		var maps []store.ModelMapping
		for _, m := range models {
			if alias, up, ok := strings.Cut(m, "="); ok {
				maps = append(maps, store.ModelMapping{Alias: alias, UpstreamModel: up})
			} else {
				maps = append(maps, store.ModelMapping{Alias: m, UpstreamModel: m})
			}
		}
		if len(maps) == 0 {
			return fmt.Errorf("at least one --model required")
		}
		id, err := st.AddSource(store.Source{
			Name: name, Type: *typ, BaseURL: *base, APIKey: apiKey,
			Priority: *priority, Weight: *weight, Enabled: true,
		}, maps)
		if err != nil {
			return err
		}
		fmt.Printf("source %q added (id %d, %d models)\n", name, id, len(maps))
		return nil

	case "list":
		srcs, err := st.ListSources()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tTYPE\tPRIORITY\tWEIGHT\tENABLED\tMODELS\tBASE")
		for _, s := range srcs {
			maps, _ := st.SourceModels(s.ID)
			aliases := make([]string, len(maps))
			for i, m := range maps {
				aliases[i] = m.Alias
				if m.UpstreamModel != m.Alias {
					aliases[i] = m.Alias + "=" + m.UpstreamModel
				}
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%v\t%s\t%s\n",
				s.ID, s.Name, s.Type, s.Priority, s.Weight, s.Enabled, strings.Join(aliases, ","), s.BaseURL)
		}
		return w.Flush()

	case "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: source remove NAME")
		}
		srcs, err := st.ListSources()
		if err != nil {
			return err
		}
		for _, s := range srcs {
			if s.Name == args[1] {
				return st.DeleteSource(s.ID)
			}
		}
		return fmt.Errorf("source %q not found", args[1])
	}
	return fmt.Errorf("unknown source subcommand %q", args[0])
}

func keyCmd(st *store.Store, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("key needs a subcommand")
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: key add NAME [--models ...] [--rpm N] [--tpm N] [--daily-tokens N]")
		}
		name := args[1]
		fs := flag.NewFlagSet("key add", flag.ExitOnError)
		models := fs.String("models", "*", "comma-separated alias whitelist or *")
		rpm := fs.Int64("rpm", 0, "requests per minute limit (0=none)")
		tpm := fs.Int64("tpm", 0, "tokens per minute limit (0=none)")
		daily := fs.Int64("daily-tokens", 0, "daily in+out token limit (0=none)")
		must(fs.Parse(args[2:]))
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: key add NAME [--models ...] [--rpm N] [--tpm N] [--daily-tokens N]")
		}
		k, err := st.AddKey(name, *models, optInt(rpm), optInt(tpm), optInt(daily))
		if err != nil {
			return err
		}
		fmt.Printf("key created for %q:\n  %s\n", k.Name, k.Key)
		return nil

	case "list":
		keys, err := st.ListKeys()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tENABLED\tSUSP\tMODELS\tRPM\tTPM\tDAILY\tAID\tCREATED")
		for _, k := range keys {
			fmt.Fprintf(w, "%d\t%s\t%v\t%v\t%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.Enabled, k.Suspended, k.Models,
				optShow(k.RPM), optShow(k.TPM), optShow(k.DailyTokens), k.AID, k.CreatedAt)
		}
		return w.Flush()

	case "revoke":
		if len(args) != 2 {
			return fmt.Errorf("usage: key revoke NAME")
		}
		return st.RevokeKey(args[1])

	case "suspend":
		if len(args) != 2 {
			return fmt.Errorf("usage: key suspend NAME  (temporary hold, e.g. agentctl freeze)")
		}
		return st.SetKeySuspended(args[1], true)

	case "resume":
		if len(args) != 2 {
			return fmt.Errorf("usage: key resume NAME")
		}
		return st.SetKeySuspended(args[1], false)

	case "bind-aid":
		if len(args) != 3 {
			return fmt.Errorf("usage: key bind-aid NAME AID  (empty AID detaches)")
		}
		return st.BindKeyAID(args[1], args[2])
	}
	return fmt.Errorf("unknown key subcommand %q", args[0])
}

func usageCmd(st *store.Store, args []string) error {
	if len(args) > 0 && args[0] == "recent" {
		limit := 20
		if len(args) >= 3 && args[1] == "--limit" {
			if n, err := strconv.Atoi(args[2]); err == nil {
				limit = n
			}
		}
		rows, err := st.RecentUsage(limit)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "TS\tREQ\tIDENT\tAID\tSOURCE\tFORMAT\tALIAS\tIN\tOUT\tCOST\tMS\tSTATUS")
		for _, r := range rows {
			cost := "-"
			if r.Cost != nil {
				cost = fmt.Sprintf("%.6f", *r.Cost)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%d\t%d\n",
				r.TS, shortReq(r.ReqID), r.KeyName, r.AID, r.SourceName, r.ClientFormat, r.ModelAlias,
				r.In, r.Out, cost, r.LatencyMS, r.Status)
		}
		return w.Flush()
	}

	since := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	if len(args) >= 2 && args[0] == "--since" {
		if d, err := time.ParseDuration(args[1]); err == nil {
			since = time.Now().UTC().Add(-d).Format(time.RFC3339Nano)
		}
	}
	byKey, byModel, bySource, byAid, err := st.SummarySince(since)
	if err != nil {
		return err
	}
	dump := func(title string, rows []store.Agg) {
		fmt.Printf("\n%s\n", title)
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "  LABEL\tREQUESTS\tIN\tOUT\tCOST$")
		for _, a := range rows {
			cost := "-"
			if a.CostEst != nil {
				cost = fmt.Sprintf("%.4f", *a.CostEst)
			}
			fmt.Fprintf(w, "  %s\t%d\t%d\t%d\t%s\n", a.Label, a.Requests, a.InTokens, a.OutTokens, cost)
		}
		w.Flush()
	}
	dump("by key:", byKey)
	dump("by model:", byModel)
	dump("by source:", bySource)
	dump("by agent (aid):", byAid)
	return nil
}

// shortReq keeps the recent-usage table narrow (req_0123456789abcdef → 0123…).
func shortReq(r string) string {
	if len(r) > 13 {
		return r[4:13] + "…"
	}
	return r
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func optInt(v *int64) *int64 {
	if v == nil || *v == 0 {
		return nil
	}
	return v
}

func optShow(v *int64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatInt(*v, 10)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
