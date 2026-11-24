# Records demo.gif: `just demo`, or `vhs demo.tape` from the repository root.
#
# scripts/demo-setup.sh builds nightshift and a throwaway repository whose
# remote looks like github.com/acme/rocket but is a local directory, with a
# stand-in gh, a throwaway signing key and the clock pinned at 17:42, so the
# recording needs no network, no account and no waiting for the night.

Output demo.gif

Require go
Require git
Require ssh-keygen

Set Shell "bash"
Set Theme "TokyoNight"
Set FontSize 15
Set Width 1120
Set Height 660
Set Padding 24
Set WindowBar Colorful
Set TypingSpeed 38ms

Hide
Type "source scripts/demo-setup.sh && clear"
Enter
Sleep 8s
Show

Sleep 500ms
Type "git status --short"
Sleep 300ms
Enter
Sleep 1500ms

Type 'nightshift commit -am "feat(parser): support nested blocks"'
Sleep 400ms
Enter
Sleep 2500ms

Type "nightshift pr create --fill --reviewer octocat --label enhancement"
Sleep 400ms
Enter
Sleep 3s

Type "nightshift status"
Sleep 300ms
Enter
Sleep 3500ms

Type "clear"
Enter
Type "# ...you log off. At 04:00 the runner wakes up:"
Sleep 600ms
Enter
Type 'NIGHTSHIFT_NOW="tomorrow 04:00" nightshift run'
Sleep 400ms
Enter
Sleep 3500ms

Type 'git log -1 --format="%h  %ad  signed: %G?  %s" --date=format:"%a %H:%M"'
Sleep 300ms
Enter
Sleep 1500ms

Type "git status --short && echo clean"
Sleep 300ms
Enter
Sleep 4s

Hide
Type "demo_teardown"
Enter
