#!/bin/bash
# The session in wherry.cast. Commands are typed out for the recording, then
# run for real against a local daemon:
#
#   make run &      # with a fresh data directory
#   ./bin/wherry login http://127.0.0.1:9000 "$(cat data/admin.token)"
#   ./bin/wherry keys:add me ~/.ssh/id_ed25519.pub
#   asciinema rec --cols 88 --rows 26 -i 1.5 -c demo/demo.sh demo/wherry.cast
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -r "$root/examples/hello-go/." "$work"
cd "$work"

PATH=$root/bin:$PATH
GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR'
export PATH GIT_SSH_COMMAND

git init -q -b main
git add -A
git commit -q -m first
git remote add wherry ssh://wherry@localhost:2222/hello

run() {
  printf '\033[1;32m$\033[0m '
  for ((i = 0; i < ${#1}; i++)); do
    printf '%s' "${1:i:1}"
    sleep 0.03
  done
  printf '\n'
  sleep 0.3
  bash -c "$1"
  sleep 1
}

run 'wherry apps:create hello'
run 'git push wherry main'
run 'curl http://hello.localhost:8080/'
sed -i 's/v1/v2/' main.go
run 'git commit -qam "say v2"'
run 'git push wherry main 2>&1 | tail -n 6'
run 'curl http://hello.localhost:8080/'
