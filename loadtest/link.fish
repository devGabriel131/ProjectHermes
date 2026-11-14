#!/usr/bin/env fish
# Seeds, inspects and removes the reusable link a k6 run uses, so runs never need hand-written SQL.
#   loadtest/link.fish seed <name>      creates a link, prints its 43-char token
#   loadtest/link.fish verify <token>   enrollment and checkout-attempt counts for that link
#   loadtest/link.fish clean <token>    deletes the link, its enrollments, students and billing rows
# Uses psql inside the Compose `db` service, against DATABASE_URL (default: .env). For Railway, point
# DATABASE_URL at a temporary TCP proxy on the Postgres service and delete the proxy afterwards.
# `clean` only touches links this script seeded (created_by 'k6...'). Stripe sandbox objects are not cleaned.
set root (path resolve (status dirname)/..)

if not set -q DATABASE_URL
    set -x DATABASE_URL (string replace -r '^DATABASE_URL=' '' (string match -r '^DATABASE_URL=.*' < $root/.env))
end

function psql_q
    docker compose -f $root/compose.yaml exec -T db psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -P pager=off $argv
end

set cmd $argv[1]
set arg $argv[2]
switch $cmd
    case seed
        if not string match -rq '^[A-Za-z0-9 _-]+$' -- "$arg"
            echo "usage: loadtest/link.fish seed <name> (letters, digits, space, _ and -)" >&2
            exit 2
        end
        set token (head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')
        psql_q -At -c "INSERT INTO links (name, token, created_at, expires_at, created_by) VALUES ('$arg', '$token', now(), now() + interval '7 days', 'k6')" >/dev/null; or exit 1
        echo $token
    case verify clean
        if not string match -rq '^[A-Za-z0-9_-]{43}$' -- "$arg"
            echo "usage: loadtest/link.fish $cmd <43-char token>" >&2
            exit 2
        end
        set link "(SELECT id FROM links WHERE token = '$arg')"
        set enrollments "(SELECT id FROM enrollments WHERE link_id = $link)"
        set contracts "(SELECT id FROM billing_contracts WHERE enrollment_id IN $enrollments)"
        if test $cmd = verify
            psql_q -c "SELECT count(*) AS enrollments, count(submitted_at) AS submitted, count(terms_accepted_at) AS accepted, count(paid_at) AS paid, (SELECT count(*) FROM students WHERE enrollment_id IN $enrollments) AS students FROM enrollments WHERE link_id = $link" \
                -c "SELECT state AS checkout_state, count(*) FROM billing_checkout_attempts WHERE contract_id IN $contracts GROUP BY 1 ORDER BY 1"
        else
            set by (psql_q -At -c "SELECT created_by FROM links WHERE token = '$arg'")
            if not string match -q 'k6*' -- "$by"
                echo "refusing: link not found or not seeded by k6 (created_by='$by')" >&2
                exit 1
            end
            psql_q -c "BEGIN" \
                -c "DELETE FROM billing_checkout_attempts WHERE contract_id IN $contracts" \
                -c "DELETE FROM billing_obligations WHERE contract_id IN $contracts" \
                -c "DELETE FROM billing_payments WHERE contract_id IN $contracts" \
                -c "DELETE FROM billing_suspensions WHERE contract_id IN $contracts" \
                -c "DELETE FROM billing_contracts WHERE enrollment_id IN $enrollments" \
                -c "DELETE FROM students WHERE enrollment_id IN $enrollments" \
                -c "DELETE FROM enrollments WHERE link_id = $link" \
                -c "DELETE FROM links WHERE token = '$arg'" \
                -c "COMMIT"
        end
    case '*'
        echo "usage: loadtest/link.fish seed <name> | verify <token> | clean <token>" >&2
        exit 2
end
