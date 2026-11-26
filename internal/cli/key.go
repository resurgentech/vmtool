package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/spf13/cobra"
)

// keyCmd is the `vmtool key` group. Its commands are exempt from the token
// gate: they are how a key is requested, shown and checked. None prompts.
func keyCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Request, show and check this client's key",
	}
	cmd.AddCommand(keyRequestCmd(e), keyStatusCmd(e), keyCheckCmd(e))
	return cmd
}

func keyRequestCmd(e *env) *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "request",
		Short: "Request a key and store it in the token file",
		Long: `Request a key: file a request with the key provider (named by server.url in
config.yml, or by --key-request-url / VMTOOL_KEY_REQUEST_URL), print where a
person approves it, and store the key in the token file. In a terminal it
waits for the approval. Anywhere else it files one request (or resumes the one
already pending), prints the notice and exits 75; run it again after approving.
With a key already in the token file it does nothing.`,
		Example: `  vmtool key request
  vmtool key request --json --role full

  Tip: with --json stdout is {"token_file": "...", "result": "present"} or
  {"token_file": "...", "result": "stored", "name": "...", "role": "..."}.
  Not in a terminal, a pending request is the error code key_pending (exit 75,
  the approval notice in message). The key is never printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tokenPath, err := config.TokenFilePath(e.dir)
			if err != nil {
				return fail("bad_request", err)
			}
			_, rerr := config.ReadTokenFile(tokenPath)
			switch {
			case rerr == nil:
				if jsonMode(cmd) {
					return emit(cmd, map[string]string{"token_file": tokenPath, "result": "present"})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "a key is already at %s\n", tokenPath)
				return nil
			case !errors.Is(rerr, config.ErrNoTokenFile):
				return fail("key_file", rerr)
			}
			res, tokenPath, err := e.requestKey(cmd, role)
			if err != nil {
				if errors.Is(err, request.ErrNoProvider) {
					return fail("bad_request", errors.New("no key request URL: set server.url in config.yml, or pass --key-request-url or set VMTOOL_KEY_REQUEST_URL"))
				}
				if lines, exit, code, ok := classifyKeyErr(err, tokenPath); ok {
					return keyErr(lines, exit, code)
				}
				return fail("key_provider", err)
			}
			if jsonMode(cmd) {
				return emit(cmd, map[string]string{"token_file": tokenPath, "result": "stored", "name": res.Name, "role": res.Role})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "key stored at %s\n", tokenPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", keyRole, "the role to ask for")
	return cmd
}

type keyPendingJSON struct {
	ApproveURL string `json:"approve_url"`
	Code       string `json:"code"`
	ExpiresAt  string `json:"expires_at"`
}

type keyStatusJSON struct {
	TokenFile        string          `json:"token_file"`
	TokenFileState   string          `json:"token_file_state"` // present, absent or refused
	TokenFileProblem string          `json:"token_file_problem,omitempty"`
	DaemonSource     string          `json:"daemon_source"` // env, .env, file or none
	StoreSource      string          `json:"store_source"`
	Pending          *keyPendingJSON `json:"pending,omitempty"`
	PendingProblem   string          `json:"pending_problem,omitempty"`
}

func keyStatusCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show where this client's key comes from and any pending request",
		Long: `Show, without any network call or change: the token file path and whether it
holds a key (present), none (absent) or is refused (wrong mode, a link, empty);
which source a call to a daemon and a call to the store would use now (env,
.env, file or none; never a value); and the pending key request, if any.`,
		Example: `  vmtool key status
  vmtool key status --json

  Tip: with --json stdout is {"token_file", "token_file_state", "daemon_source",
  "store_source"} plus "pending": {"approve_url", "code", "expires_at"} while a
  request is open. No key or hash is ever shown.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			serviceURL, override, tokenPath, err := e.keyRequestParams(cmd)
			if err != nil {
				return fail("bad_request", err)
			}
			st := keyStatusJSON{TokenFile: tokenPath, DaemonSource: config.SourceNone, StoreSource: config.SourceNone}
			_, ferr := config.ReadTokenFile(tokenPath)
			switch {
			case ferr == nil:
				st.TokenFileState = "present"
			case errors.Is(ferr, config.ErrNoTokenFile):
				st.TokenFileState = "absent"
			default:
				st.TokenFileState, st.TokenFileProblem = "refused", ferr.Error()
			}
			if _, src, err := config.DaemonKey(e.dir); err == nil {
				st.DaemonSource = src
			}
			if _, src, err := config.StoreKey(e.dir); err == nil {
				st.StoreSource = src
			}
			n, ok, perr := request.Pending(request.Options{ServiceURL: serviceURL, Override: override, TokenPath: tokenPath})
			switch {
			case ok:
				st.Pending = &keyPendingJSON{ApproveURL: n.ApproveURL, Code: n.UserCode, ExpiresAt: n.ExpiresAt.UTC().Format(time.RFC3339)}
			case perr != nil && !errors.Is(perr, request.ErrNoProvider):
				st.PendingProblem = perr.Error()
			}
			if jsonMode(cmd) {
				return emit(cmd, st)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "token file: %s (%s)\n", st.TokenFile, st.TokenFileState)
			if st.TokenFileProblem != "" {
				fmt.Fprintf(w, "  %s\n", st.TokenFileProblem)
			}
			fmt.Fprintf(w, "daemon key: %s\nstore key: %s\n", st.DaemonSource, st.StoreSource)
			if st.Pending != nil {
				fmt.Fprintf(w, "pending request: approve at %s, check the code %s, expires %s\n", st.Pending.ApproveURL, st.Pending.Code, st.Pending.ExpiresAt)
			}
			if st.PendingProblem != "" {
				fmt.Fprintf(w, "pending request: %s\n", st.PendingProblem)
			}
			return nil
		},
	}
}

// keyTarget is one row of `key check`.
type keyTarget struct {
	Target string `json:"target"`
	Kind   string `json:"kind"`   // local, direct, forwarded or store
	Result string `json:"result"` // accepted, refused or unreachable
	Detail string `json:"detail,omitempty"`
}

// verdict turns the status of an authenticated GET into a row's result: 2xx
// and a 404 (a daemon without the route answers 404 only after it accepted the
// key) are accepted; 401 and 403 refused; anything else tells nothing about
// the key.
func verdict(status int) (result, detail string) {
	switch {
	case status >= 200 && status < 300, status == http.StatusNotFound:
		return "accepted", ""
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "refused", ""
	}
	return "unreachable", fmt.Sprintf("HTTP %d", status)
}

func keyCheckCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Present the token file's key to the local service, each host and the store",
		Long: `Present the key in the token file (whatever the order would pick for a command)
to: the local service, each host's daemon directly, each host through the local
service, and the store's whoami. One row per target: accepted, refused or
unreachable. Exit 0 only when every reachable target accepted the key (and at
least one did), else 1; an unreachable target does not fail the check. The rows
are printed either way. Each target has a 5 s limit.`,
		Example: `  vmtool key check
  vmtool key check --json

  Tip: with --json stdout is {"token_file", "ok", "targets": [{"target", "kind",
  "result", "detail"}]}; kind is local, direct, forwarded or store. The key is
  never printed. Use it after 'vmtool key request' to see every service
  accepts the new key before the old tokens are removed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tokenPath, err := config.TokenFilePath(e.dir)
			if err != nil {
				return fail("bad_request", err)
			}
			key, err := config.ReadTokenFile(tokenPath)
			switch {
			case errors.Is(err, config.ErrNoTokenFile):
				return fail("key_file", fmt.Errorf("no key at %s; run 'vmtool key request'", tokenPath))
			case err != nil:
				return fail("key_file", err)
			}
			cfg, err := e.loadConfig()
			if err != nil {
				return fail("bad_request", err)
			}
			rows := checkTargets(cmdContext(cmd), cfg, key)
			// Exit 0 only when every reachable target accepted the key; an
			// unreachable target (a host that is asleep, say) does not fail
			// the check, but at least one target must have accepted it.
			ok, accepted := true, false
			for _, r := range rows {
				ok = ok && r.Result != "refused"
				accepted = accepted || r.Result == "accepted"
			}
			ok = ok && accepted
			if jsonMode(cmd) {
				if err := emit(cmd, map[string]any{"token_file": tokenPath, "ok": ok, "targets": rows}); err != nil {
					return err
				}
			} else {
				w := cmd.OutOrStdout()
				for _, r := range rows {
					line := fmt.Sprintf("%-10s %-10s %s", r.Kind, r.Result, r.Target)
					if r.Detail != "" {
						line += "  (" + r.Detail + ")"
					}
					fmt.Fprintln(w, line)
				}
			}
			if !ok {
				return fail("key_check_failed", errors.New("not every target accepted the key at "+tokenPath))
			}
			return nil
		},
	}
}

// checkTargets presents key to every target, in parallel, and returns the rows
// in a fixed order: local, direct per host, forwarded per host, store.
func checkTargets(ctx context.Context, cfg *config.Config, key string) []keyTarget {
	names := make([]string, 0, len(cfg.Hosts))
	for n := range cfg.Hosts {
		names = append(names, n)
	}
	sort.Strings(names)

	local := "https://" + localAddr(cfg)
	var jobs []func() keyTarget
	get := func(hc *http.Client, base, path string) func() (int, error) {
		return func() (int, error) {
			c, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			st, _, err := probe(c, hc, base+path, key)
			return st, err
		}
	}
	row := func(kind, target string, f func() (int, error)) func() keyTarget {
		return func() keyTarget {
			st, err := f()
			if err != nil {
				return keyTarget{Target: target, Kind: kind, Result: "unreachable", Detail: errText(err)}
			}
			res, d := verdict(st)
			return keyTarget{Target: target, Kind: kind, Result: res, Detail: d}
		}
	}
	lc := localClient(cfg, probeTimeout)
	jobs = append(jobs, row("local", localAddr(cfg), get(lc, local, "/v1/personal-keys")))
	for _, n := range names {
		var tc = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: keyProbeRoots}
		jobs = append(jobs, row("direct", n, get(probeClient(tc, probeTimeout), hosts.BaseURL(cfg.Hosts[n]), "/v1/personal-keys")))
	}
	for _, n := range names {
		// The workstation forwards the caller's key to the host: a cheap read.
		jobs = append(jobs, row("forwarded", n, get(lc, local, "/v1/hosts/"+n+"/pools")))
	}
	if cfg.StoreConfigured() {
		jobs = append(jobs, func() keyTarget {
			t := keyTarget{Target: cfg.Server.URL, Kind: "store"}
			c, err := storeclient.New(cfg.Server.URL, key, storeClientOptions...)
			if err != nil {
				t.Result, t.Detail = "unreachable", err.Error()
				return t
			}
			defer c.Close()
			cx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			name, err := c.Whoami(cx)
			switch {
			case err == nil:
				t.Result, t.Detail = "accepted", "caller "+name
			case errors.Is(err, storeclient.ErrUnauthorized), errors.Is(err, storeclient.ErrForbidden):
				t.Result = "refused"
			case errors.Is(err, storeclient.ErrNotFound):
				t.Result, t.Detail = "accepted", "no whoami route"
			default:
				t.Result, t.Detail = "unreachable", errText(err)
			}
			return t
		})
	}
	out := make([]keyTarget, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = j()
		}()
	}
	wg.Wait()
	return out
}

// errText is a transport error cut to one short line.
func errText(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
