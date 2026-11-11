#!/usr/bin/env fish

argparse --stop-nonopt h/help 'e/environment=' -- $argv; or exit 2

if set -q _flag_help; or not set -q argv[1]
    printf '%s\n' 'Usage: ./user.fish [--environment NAME] <command> [username]

Commands:
  add <username>     Create a staff account.
  passwd <username>  Change a password and sign out its sessions.
  revoke <username>  Sign out every session for an account.
  remove <username>  Delete an account.
  list               List account usernames.

Runs deployed ProjectHermes in amused-light, production by default.
Requires Railway CLI login and a registered SSH key, not local DATABASE_URL.
Passwords are entered twice at hidden interactive terminal prompts.'
    exit 0
end

set -q _flag_environment; or set -l _flag_environment production

if contains -- $argv[1] add passwd
    if not isatty stdin; or not isatty stdout
        printf '%s\n' 'Password commands require interactive stdin and stdout; run this launcher directly in your terminal.' >&2
        exit 2
    end
end

exec railway ssh \
    --project a0207263-4a1b-4174-b693-5830fd46e486 \
    --environment "$_flag_environment" \
    --service ProjectHermes \
    -- /app/out user $argv
