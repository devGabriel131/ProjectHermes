#!/usr/bin/env fish
# Runs one loadtest script in Docker with the live dashboard on and a summary of why requests failed.
#   loadtest/run.fish capacity.js BASE_URL=http://127.0.0.1:3000 TOKEN=... [WINDOW=60 ...]
# Live view: http://127.0.0.1:5665 while it runs. Afterwards in /tmp/k6/out: <script>.html (dashboard
# report), <script>.json (full summary), <script>.log (console output, including every FAIL line).
set root (path resolve (status dirname)/..)
set script $argv[1]
set name (path basename --no-extension $script)
set out /tmp/k6/out

if test -z "$script"; or not test -f $root/loadtest/$script
    echo "usage: loadtest/run.fish <script.js> [KEY=value ...]" >&2
    exit 2
end

mkdir -p $out
chmod 777 $out # the k6 container writes its summary here as another user

set envs -e K6_WEB_DASHBOARD=true -e K6_WEB_DASHBOARD_EXPORT=$out/$name.html -e K6_WEB_DASHBOARD_PERIOD=2s
for kv in $argv[2..-1]
    set -a envs -e $kv
end

# FAIL lines only go to the log: a bad run can produce thousands, and they are summarized below.
docker run --rm --network host --ulimit nofile=1048576:1048576 -v $root/loadtest:/loadtest -v /tmp/k6:/tmp/k6 \
    $envs -e OUT=$out/$name.json grafana/k6:latest run -q /loadtest/$script 2>&1 | tee $out/$name.log | grep -v 'msg="FAIL '
set rc $pipestatus[1]

# FAIL <who> <label> status=<n> reason=<reason> [alert="..."], counted without the customer index.
set fails (string replace -rf '.*msg="FAIL \S+ (.*)" source=console$' '$1' < $out/$name.log)
if set -q fails[1]
    echo
    echo "failure causes (count, step, status, reason):"
    printf '%s\n' $fails | sort | uniq -c | sort -rn | head -20
end
exit $rc
