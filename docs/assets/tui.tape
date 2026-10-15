# Regenerate: see docs/assets/demo-setup.sh (builds the isolated fake-agent state), then:
#   docs/assets/demo-setup.sh up ./otag && vhs docs/assets/tui.tape ; docs/assets/demo-setup.sh down
Output docs/assets/tui.gif

Set Shell "bash"
Set Width 1200
Set Height 700
Set FontSize 15
Set Theme "Dracula"
Set Padding 8
Set Framerate 12
Set TypingSpeed 40ms

Hide
Type "export PS1='$ ' && . /tmp/otag-demo/env.sh && cd /tmp/otag-demo/acme-api && clear"
Enter
Sleep 500ms
Show

Type "otag tui"
Sleep 500ms
Enter
Sleep 3s

# Walk the task tree; the preview on the right follows the selection.
Down
Sleep 1s
Down
Sleep 1s
Down
Sleep 1s
Down
Sleep 2s

# Needs tab: the open question
Type "!"
Sleep 3s
Type "1"
Sleep 500ms

# A running worker with live output
Down 4
Sleep 1500ms
Down 2
Sleep 4s

# Details tab
Type "2"
Sleep 2500ms
Type "1"
Sleep 500ms

# Context menu
Type "o"
Sleep 2500ms
Escape
Sleep 500ms

# Help overlay
Type "?"
Sleep 3s
Escape
Sleep 1s

Type "q"
Sleep 500ms
