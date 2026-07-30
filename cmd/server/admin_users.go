package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

type adminUsersCommand struct {
	List     adminUsersListCommand     `cmd:"" help:"List users."`
	Create   adminUsersCreateCommand   `cmd:"" help:"Create a user and API key."`
	Get      adminUsersGetCommand      `cmd:"" help:"Get a user."`
	Update   adminUsersUpdateCommand   `cmd:"" help:"Rename a user."`
	Enable   adminUsersEnableCommand   `cmd:"" help:"Enable a user."`
	Disable  adminUsersDisableCommand  `cmd:"" help:"Disable a user."`
	ResetKey adminUsersResetKeyCommand `cmd:"" name:"reset-key" help:"Replace a user's API key."`
}

type adminUsersListCommand struct {
	Enabled string `help:"Filter by enabled state."`
}

type adminUsersCreateCommand struct {
	Name string `arg:"" name:"name" help:"User name."`
}

type adminUsersGetCommand struct {
	UserID string `arg:"" name:"user_id" help:"User ID."`
}

type adminUsersUpdateCommand struct {
	UserID string `arg:"" name:"user_id" help:"User ID."`
	Name   string `help:"New user name."`
}

type adminUsersEnableCommand struct {
	UserID string `arg:"" name:"user_id" help:"User ID."`
}

type adminUsersDisableCommand struct {
	UserID string `arg:"" name:"user_id" help:"User ID."`
}

type adminUsersResetKeyCommand struct {
	UserID string `arg:"" name:"user_id" help:"User ID."`
}

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

func (c *adminUsersListCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	query := url.Values{}
	if strings.TrimSpace(c.Enabled) != "" {
		query.Set("enabled", strings.TrimSpace(c.Enabled))
	}
	var payload usersListResponse
	if err := client.doJSON(runtime.ctx, http.MethodGet, "/users", query, nil, &payload); err != nil {
		return err
	}
	if admin.JSON {
		return writeJSONOutput(runtime.stdout, payload)
	}
	return writeUsersTable(runtime.stdout, payload.Users)
}

func (c *adminUsersCreateCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	var created codexonly.CreatedUserAPIKey
	if err := client.doJSON(runtime.ctx, http.MethodPost, "/users", nil, userCreateRequest{Name: c.Name}, &created); err != nil {
		return err
	}
	if admin.JSON {
		return writeJSONOutput(runtime.stdout, created)
	}
	return writeCreatedUserTable(runtime.stdout, created)
}

func (c *adminUsersGetCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(runtime.ctx, http.MethodGet, "/users/"+url.PathEscape(c.UserID), nil, nil, &user); err != nil {
		return err
	}
	if admin.JSON {
		return writeJSONOutput(runtime.stdout, user)
	}
	return writeUsersTable(runtime.stdout, []codexonly.UserWithAPIKey{user})
}

func (c *adminUsersUpdateCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("--name is required")
	}
	req := userUpdateRequest{Name: &c.Name}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(runtime.ctx, http.MethodPatch, "/users/"+url.PathEscape(c.UserID), nil, req, &user); err != nil {
		return err
	}
	if admin.JSON {
		return writeJSONOutput(runtime.stdout, user)
	}
	return writeUsersTable(runtime.stdout, []codexonly.UserWithAPIKey{user})
}

func (c *adminUsersEnableCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	enabled := true
	req := userUpdateRequest{Enabled: &enabled}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(runtime.ctx, http.MethodPatch, "/users/"+url.PathEscape(c.UserID), nil, req, &user); err != nil {
		return err
	}
	if admin.JSON {
		return writeJSONOutput(runtime.stdout, user)
	}
	return writeUsersTable(runtime.stdout, []codexonly.UserWithAPIKey{user})
}

func (c *adminUsersDisableCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	enabled := false
	req := userUpdateRequest{Enabled: &enabled}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(runtime.ctx, http.MethodPatch, "/users/"+url.PathEscape(c.UserID), nil, req, &user); err != nil {
		return err
	}
	if admin.JSON {
		return writeJSONOutput(runtime.stdout, user)
	}
	return writeUsersTable(runtime.stdout, []codexonly.UserWithAPIKey{user})
}

func (c *adminUsersResetKeyCommand) Run(runtime *commandRuntime, client *adminClient, admin *adminCommand) error {
	var created codexonly.CreatedUserAPIKey
	if err := client.doJSON(runtime.ctx, http.MethodPost, "/users/"+url.PathEscape(c.UserID)+"/api-key/reset", nil, nil, &created); err != nil {
		return err
	}
	if admin.JSON {
		return writeJSONOutput(runtime.stdout, created)
	}
	return writeCreatedUserTable(runtime.stdout, created)
}

func writeCreatedUserTable(stdout io.Writer, created codexonly.CreatedUserAPIKey) error {
	if _, err := fmt.Fprintln(stdout, "USER_ID\tNAME\tENABLED\tAPI_KEY_ID\tMASKED_KEY\tAPI_KEY_VALUE"); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "%s\t%s\t%t\t%s\t%s\t%s\n", created.User.ID, created.User.Name, created.User.Enabled, created.APIKey.ID, created.APIKey.MaskedKey, created.PlaintextAPIKey)
	return err
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
