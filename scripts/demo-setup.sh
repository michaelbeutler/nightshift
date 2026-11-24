#!/usr/bin/env bash
# Prepares a shell for recording demo.gif; demo.tape sources it while hidden.
#
# It builds nightshift and a throwaway repository, "rocket", whose origin
# looks like git@github.com:acme/rocket.git but pushes to a local bare
# repository (pushInsteadOf). A stand-in gh on PATH answers like the real one
# and "opens" pull request #42, commits are signed with a throwaway SSH key,
# and the clock is pinned at 17:42 today (NIGHTSHIFT_NOW), so the recording
# needs no network, no account and no waiting for the night. Your own git
# configuration and nightshift state are not touched. Source it from the
# repository root:
#
#   source scripts/demo-setup.sh
#
# demo_teardown removes everything again.

demo_root=$(pwd)
demo_dir=$(mktemp -d)
mkdir -p "$demo_dir/bin"
go build -o "$demo_dir/bin/nightshift" . || return 1

cat >"$demo_dir/bin/gh" <<'GH'
#!/bin/sh
# A stand-in for gh: logged in, and opening pull request #42.
case "$1 $2" in
"auth status") exit 0 ;;
"pr create") cat >/dev/null && echo "https://github.com/acme/rocket/pull/42" ;;
*) echo "demo gh: $*" >&2 && exit 1 ;;
esac
GH
chmod +x "$demo_dir/bin/gh"

ssh-keygen -q -t ed25519 -N "" -C demo -f "$demo_dir/signing-key" || return 1
echo "ada@example.com $(cat "$demo_dir/signing-key.pub")" >"$demo_dir/allowed_signers"
export GIT_CONFIG_GLOBAL="$demo_dir/gitconfig" GIT_CONFIG_NOSYSTEM=1
cat >"$GIT_CONFIG_GLOBAL" <<GITCONFIG
[init]
	defaultBranch = main
[user]
	name = Ada Lovelace
	email = ada@example.com
	signingKey = $demo_dir/signing-key.pub
[gpg]
	format = ssh
[gpg "ssh"]
	allowedSignersFile = $demo_dir/allowed_signers
[commit]
	gpgSign = true
[url "$demo_dir/remote.git"]
	pushInsteadOf = git@github.com:acme/rocket.git
GITCONFIG

git init -q --bare "$demo_dir/remote.git"
git init -q "$demo_dir/rocket"
cd "$demo_dir/rocket" || return 1
git remote add origin git@github.com:acme/rocket.git
mkdir -p src
cat >src/parser.go <<'GO'
package parser

// Parse reads one block.
func Parse(src string) Block {
	return Block{Text: src}
}
GO
printf '# rocket\n\nA tiny parser.\n' >README.md
git add . && git commit -q -m "feat: a tiny parser"
git push -q origin main 2>/dev/null
git symbolic-ref refs/remotes/origin/HEAD refs/remotes/origin/main
git switch -q -c feat/nested-blocks
cat >src/parser.go <<'GO'
package parser

// Parse reads a block and the blocks nested in it.
func Parse(src string) Block {
	b := Block{Text: src}
	for _, inner := range split(src) {
		b.Children = append(b.Children, Parse(inner))
	}
	return b
}
GO
printf '# rocket\n\nA tiny parser, now with nested blocks.\n' >README.md

export NIGHTSHIFT_HOME="$demo_dir/home"
export NIGHTSHIFT_NOW="$(date +%Y-%m-%d) 17:42"
# A narrow window, so the commit, push and PR are all due by the 04:00 run.
export NIGHTSHIFT_SCHEDULE_WINDOW="01:00-02:00"
export PATH="$demo_dir/bin:$PATH"
export PS1='\[\e[38;5;141m\]rocket\[\e[0m\] \[\e[2m\]$(git branch --show-current 2>/dev/null)\[\e[0m\] \[\e[38;5;141m\]❯\[\e[0m\] '

demo_teardown() {
	cd "$demo_root" || return
	rm -rf "$demo_dir"
	unset GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM NIGHTSHIFT_HOME NIGHTSHIFT_NOW NIGHTSHIFT_SCHEDULE_WINDOW
}
