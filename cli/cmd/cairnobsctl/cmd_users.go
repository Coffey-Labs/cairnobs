// Command surface for api/localauth -- single-tenant mode's local
// username/password login and user manager (see /docs -- deployment
// runbook, and api/localauth's package doc comment for the full
// feature). Same list/create/delete shape as agents/dashboards, plus a
// "login" subcommand: unlike every other resource this CLI manages,
// there's no way to get a first CAIRNOBSCTL_TOKEN without one.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func cmdUsers(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "cairnobsctl users: expected a subcommand (login, list, create, delete, reset-password)")
		return 1
	}
	apiURL, rest := extractAPIFlag(args[1:], os.Getenv)
	token := resolveToken(os.Getenv)

	switch args[0] {
	case "login":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "cairnobsctl users login: missing username")
			return 1
		}
		return cmdUsersLogin(rest[0], rest[1:], apiURL, stdin, stdout, stderr)
	case "list":
		return httpGetJSON(apiURL, "/auth/users", token, stdout, stderr)
	case "create":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "cairnobsctl users create: missing username")
			return 1
		}
		return cmdUsersCreate(rest[0], rest[1:], apiURL, token, stdin, stdout, stderr)
	case "delete":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "cairnobsctl users delete: missing user id")
			return 1
		}
		return httpMutateNoBody(http.MethodDelete, apiURL, "/auth/users/"+rest[0], token, "", "user deleted", stdout, stderr)
	case "reset-password":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "cairnobsctl users reset-password: missing user id")
			return 1
		}
		return cmdUsersResetPassword(rest[0], rest[1:], apiURL, token, stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "cairnobsctl users: unknown subcommand %q (want login, list, create, delete, reset-password)\n", args[0])
		return 1
	}
}

// extractPasswordStdinFlag pulls the boolean --password-stdin flag out
// of args if present -- same "walk args, splice out the one flag this
// caller cares about" shape extractAPIFlag already uses at the
// top-level dispatch layer. Unlike the --password <value> flag this
// replaced (security-audit finding L-4), this flag never carries the
// secret itself -- only readPasswordFromStdin's caller decides to
// actually read one, same "docker login --password-stdin" convention,
// chosen over inventing a new one: a plaintext password passed as a CLI
// argument is visible to any other local user via `ps`/
// `/proc/<pid>/cmdline` and typically lands in shell history too.
func extractPasswordStdinFlag(args []string) (useStdin bool, rest []string) {
	for _, a := range args {
		if a == "--password-stdin" {
			useStdin = true
			continue
		}
		rest = append(rest, a)
	}
	return useStdin, rest
}

// readPasswordFromStdin reads a single line from stdin. Not masked
// (this codebase has no terminal/raw-mode dependency to draw on -- see
// resolveToken's doc comment for the same tradeoff already accepted for
// CAIRNOBSCTL_TOKEN); pipe the value in (`echo "$PW" | cairnobsctl users
// login admin`) rather than typing it at an interactive terminal where
// that matters.
func readPasswordFromStdin(stdin io.Reader) (string, error) {
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

type loginRequestBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponseBody struct {
	Token string `json:"token"`
	Error string `json:"error"`
}

// cmdUsersLogin prints only the raw token to stdout on success (nothing
// else) -- deliberately pipeable: `export CAIRNOBSCTL_TOKEN=$(cairnobsctl
// users login admin)`.
func cmdUsersLogin(username string, _ []string, apiURL string, stdin io.Reader, stdout, stderr io.Writer) int {
	password, err := readPasswordFromStdin(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "reading password: %v\n", err)
		return 1
	}

	body, err := json.Marshal(loginRequestBody{Username: username, Password: password})
	if err != nil {
		fmt.Fprintf(stderr, "encoding request: %v\n", err)
		return 1
	}
	req, err := http.NewRequest(http.MethodPost, apiURL+"/auth/login", strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintf(stderr, "building request: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "reading response: %v\n", err)
		return 1
	}
	var login loginResponseBody
	_ = json.Unmarshal(respBody, &login)
	if resp.StatusCode != http.StatusOK {
		if login.Error != "" {
			fmt.Fprintf(stderr, "login failed: %s\n", login.Error)
		} else {
			fmt.Fprintf(stderr, "login failed: status %d\n", resp.StatusCode)
		}
		return 1
	}
	fmt.Fprintln(stdout, login.Token)
	return 0
}

func cmdUsersCreate(username string, flagArgs []string, apiURL, token string, stdin io.Reader, stdout, stderr io.Writer) int {
	role := "editor"
	for i := 0; i < len(flagArgs); i++ {
		if flagArgs[i] == "--role" && i+1 < len(flagArgs) {
			role = flagArgs[i+1]
			i++
			continue
		}
	}

	password, err := readPasswordFromStdin(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "reading password: %v\n", err)
		return 1
	}

	body, err := json.Marshal(struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}{Username: username, Password: password, Role: role})
	if err != nil {
		fmt.Fprintf(stderr, "encoding request: %v\n", err)
		return 1
	}
	return httpPostJSON(apiURL, "/auth/users", token, string(body), stdout, stderr)
}

// cmdUsersResetPassword defaults to requesting a server-generated
// random password (empty body -- see api/localauth's handleResetPassword
// doc comment): pass --password-stdin to instead set a specific password
// read from stdin. There is deliberately no --password <value> flag (see
// extractPasswordStdinFlag's doc comment) -- a specific password chosen
// this way must be piped in, never typed as a bare CLI argument.
func cmdUsersResetPassword(id string, flagArgs []string, apiURL, token string, stdin io.Reader, stdout, stderr io.Writer) int {
	useStdin, _ := extractPasswordStdinFlag(flagArgs)
	body := "{}"
	if useStdin {
		password, err := readPasswordFromStdin(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "reading password: %v\n", err)
			return 1
		}
		encoded, err := json.Marshal(struct {
			Password string `json:"password"`
		}{Password: password})
		if err != nil {
			fmt.Fprintf(stderr, "encoding request: %v\n", err)
			return 1
		}
		body = string(encoded)
	}
	return httpPostJSON(apiURL, "/auth/users/"+id+"/reset-password", token, body, stdout, stderr)
}
