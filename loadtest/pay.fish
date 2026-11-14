#!/usr/bin/env fish
# Pays the Checkout Sessions a capacity.js run created, by typing sandbox cards into Stripe's hosted page
# in a headless browser (Playwright in Docker; the first run pulls the ~2 GB image and installs the package).
#   loadtest/pay.fish [LOG=/tmp/k6/out/capacity.log] [CONCURRENCY=3] [CARDS=4242...,5555...]
# Prints PAID/FAILED per customer. The sandbox then sends webhooks to the app's PUBLIC_BASE_URL, so run this
# against sessions made by a deployed app (a local app never hears Stripe) for the payments to land in its database.
set root (path resolve (status dirname)/..)
set playwright 1.65.1

set envs
for kv in $argv
    set -a envs -e $kv
end

docker run --rm --network host --ipc=host -v $root/loadtest:/loadtest:ro -v /tmp/k6:/tmp/k6 $envs \
    mcr.microsoft.com/playwright:v$playwright-noble \
    sh -c "mkdir -p /tmp/pw && cd /tmp/pw && npm i --no-audit --no-fund --silent playwright@$playwright && NODE_PATH=/tmp/pw/node_modules node /loadtest/pay.cjs"
