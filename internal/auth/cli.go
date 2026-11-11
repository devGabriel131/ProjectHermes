package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
	"gorm.io/gorm"
)

// Usage documents every command. Passwords are only read from hidden terminal
// prompts, never from arguments, environment variables, or piped input.
const Usage = `Usage:
  projecthermes                         Start the web server.
  projecthermes user add <username>     Create a staff account.
  projecthermes user passwd <username>  Change a password and sign out its sessions.
  projecthermes user revoke <username>  Sign out every session for an account.
  projecthermes user remove <username>  Delete an account.
  projecthermes user list               List account usernames.

Usernames are 1-64 ASCII letters, digits, ".", "_" or "-" and are case-insensitive.
Passwords: at least 4 characters, at most 1024 bytes; entered twice at a hidden terminal prompt.
`

var errUsage = errors.New("invalid command\n\n" + strings.TrimSuffix(Usage, "\n"))

// RunCLI runs the account command in args, the arguments after "user".
func RunCLI(ctx context.Context, db *gorm.DB, args []string) error {
	if len(args) == 1 && args[0] == "list" {
		return listUsers(ctx, db)
	}
	if len(args) != 2 {
		return errUsage
	}
	username, ok := normalizeUsername(args[1])
	if !ok {
		return errors.New(`username must be 1-64 ASCII letters, digits, ".", "_" or "-"`)
	}
	switch args[0] {
	case "add", "passwd":
		password, err := readNewPassword()
		if err != nil {
			return err
		}
		if args[0] == "add" {
			return createUser(ctx, db, username, password)
		}
		return setPassword(ctx, db, username, password)
	case "revoke":
		return revokeSessions(ctx, db, username)
	case "remove":
		return removeUser(ctx, db, username)
	}
	return errUsage
}

func listUsers(ctx context.Context, db *gorm.DB) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var usernames []string
	if err := db.WithContext(ctx).Model(&User{}).Order("username").Pluck("username", &usernames).Error; err != nil {
		return err
	}
	for _, username := range usernames {
		if _, err := fmt.Println(username); err != nil {
			return err
		}
	}
	return nil
}

func readNewPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("passwords must be typed at an interactive terminal; run this command in an interactive SSH shell")
	}
	password, err := promptPassword(fd, "New password: ")
	if err != nil {
		return "", err
	}
	// Temporary development minimum; raise it before production.
	if len(password) > 1024 || utf8.RuneCount(password) < 4 {
		return "", errors.New("password must be at least 4 characters and at most 1024 bytes")
	}
	confirmation, err := promptPassword(fd, "Confirm password: ")
	if err != nil {
		return "", err
	}
	if !bytes.Equal(password, confirmation) {
		return "", errors.New("passwords do not match")
	}
	return string(password), nil
}

func promptPassword(fd int, prompt string) ([]byte, error) {
	fmt.Fprint(os.Stderr, prompt)
	password, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("read password: %w", err)
	}
	return password, nil
}
