package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

type usersListResponse struct {
	Users []codexonly.UserWithAPIKey `json:"users"`
}

type userCreateRequest struct {
	Name string `json:"name"`
}

type userUpdateRequest struct {
	Name    *string `json:"name,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

func runAdminUsers(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("admin users requires a command")
	}
	switch args[0] {
	case "list":
		return runAdminUsersList(ctx, client, opts, args[1:], stdout)
	case "create":
		return runAdminUsersCreate(ctx, client, opts, args[1:], stdout)
	case "get":
		return runAdminUsersGet(ctx, client, opts, args[1:], stdout)
	case "update":
		return runAdminUsersUpdate(ctx, client, opts, args[1:], stdout)
	case "enable":
		return runAdminUsersSetEnabled(ctx, client, opts, args[1:], stdout, true)
	case "disable":
		return runAdminUsersSetEnabled(ctx, client, opts, args[1:], stdout, false)
	case "reset-key":
		return runAdminUsersResetKey(ctx, client, opts, args[1:], stdout)
	default:
		return fmt.Errorf("unknown admin users command %q", args[0])
	}
}

func runAdminUsersList(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin users list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	enabled := fs.String("enabled", "", "Filter by enabled state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := url.Values{}
	if strings.TrimSpace(*enabled) != "" {
		query.Set("enabled", strings.TrimSpace(*enabled))
	}
	var payload usersListResponse
	if err := client.doJSON(ctx, http.MethodGet, "/users", query, nil, &payload); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, payload)
	}
	return writeUsersTable(stdout, payload.Users)
}

func runAdminUsersCreate(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: admin users create <name>")
	}
	var created codexonly.CreatedUserAPIKey
	if err := client.doJSON(ctx, http.MethodPost, "/users", nil, userCreateRequest{Name: args[0]}, &created); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, created)
	}
	fmt.Fprintf(stdout, "USER_ID\tNAME\tENABLED\tAPI_KEY_ID\tMASKED_KEY\tAPI_KEY_VALUE\n")
	fmt.Fprintf(stdout, "%s\t%s\t%t\t%s\t%s\t%s\n", created.User.ID, created.User.Name, created.User.Enabled, created.APIKey.ID, created.APIKey.MaskedKey, created.PlaintextAPIKey)
	return nil
}

func runAdminUsersGet(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: admin users get <user_id>")
	}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(ctx, http.MethodGet, "/users/"+url.PathEscape(args[0]), nil, nil, &user); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, user)
	}
	return writeUsersTable(stdout, []codexonly.UserWithAPIKey{user})
}

func runAdminUsersUpdate(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	name, rest, err := extractStringFlag(args, "--name")
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: admin users update <user_id> --name <name>")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("--name is required")
	}
	req := userUpdateRequest{Name: &name}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(ctx, http.MethodPatch, "/users/"+url.PathEscape(rest[0]), nil, req, &user); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, user)
	}
	return writeUsersTable(stdout, []codexonly.UserWithAPIKey{user})
}

func runAdminUsersSetEnabled(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer, enabled bool) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: admin users enable|disable <user_id>")
	}
	req := userUpdateRequest{Enabled: &enabled}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(ctx, http.MethodPatch, "/users/"+url.PathEscape(args[0]), nil, req, &user); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, user)
	}
	return writeUsersTable(stdout, []codexonly.UserWithAPIKey{user})
}

func runAdminUsersResetKey(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: admin users reset-key <user_id>")
	}
	var created codexonly.CreatedUserAPIKey
	if err := client.doJSON(ctx, http.MethodPost, "/users/"+url.PathEscape(args[0])+"/api-key/reset", nil, nil, &created); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, created)
	}
	fmt.Fprintf(stdout, "USER_ID\tNAME\tENABLED\tAPI_KEY_ID\tMASKED_KEY\tAPI_KEY_VALUE\n")
	fmt.Fprintf(stdout, "%s\t%s\t%t\t%s\t%s\t%s\n", created.User.ID, created.User.Name, created.User.Enabled, created.APIKey.ID, created.APIKey.MaskedKey, created.PlaintextAPIKey)
	return nil
}

func writeUsersTable(stdout io.Writer, users []codexonly.UserWithAPIKey) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tUSER_ID\tENABLED\tAPI_KEY\tMASKED_KEY\tKEY_ENABLED")
	for _, item := range users {
		keyID := ""
		masked := ""
		keyEnabled := ""
		if item.APIKey != nil {
			keyID = item.APIKey.ID
			masked = item.APIKey.MaskedKey
			keyEnabled = fmt.Sprintf("%t", item.APIKey.Enabled)
		}
		fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\t%s\n", item.User.Name, item.User.ID, item.User.Enabled, keyID, masked, keyEnabled)
	}
	return tw.Flush()
}

func extractStringFlag(args []string, name string) (string, []string, error) {
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == name:
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a value", name)
			}
			i++
			return args[i], append(rest, args[i+1:]...), nil
		case strings.HasPrefix(arg, name+"="):
			return strings.TrimPrefix(arg, name+"="), append(rest, args[i+1:]...), nil
		default:
			rest = append(rest, arg)
		}
	}
	return "", rest, nil
}
