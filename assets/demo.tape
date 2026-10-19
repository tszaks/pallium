# Records assets/demo.gif with https://github.com/charmbracelet/vhs.
# Uses a fresh clone and an empty HOME so no local session history appears:
#   git clone . /tmp/pallium-demo && mkdir -p /tmp/pallium-demohome
#   (cd /tmp/pallium-demo && HOME=/tmp/pallium-demohome pallium index)
#   vhs assets/demo.tape
Output assets/demo.gif
Set Shell "bash"
Set FontFamily "Menlo"
Set FontSize 15
Set Width 1100
Set Height 640
Set Padding 28
Set LineHeight 1.25
Set TypingSpeed 45ms
Set Theme { "name": "pallium", "background": "#0a0a0a", "foreground": "#ededed", "cursor": "#ededed", "selection": "#2e2e2e", "black": "#0a0a0a", "brightBlack": "#666666", "red": "#ff5c5c", "brightRed": "#ff5c5c", "green": "#00c26e", "brightGreen": "#00c26e", "yellow": "#f5a623", "brightYellow": "#f5a623", "blue": "#a1a1a1", "brightBlue": "#ededed", "magenta": "#a1a1a1", "brightMagenta": "#ededed", "cyan": "#a1a1a1", "brightCyan": "#ededed", "white": "#ededed", "brightWhite": "#ffffff" }

Hide
Type "export HOME=/tmp/pallium-demohome PS1='$ ' && cd /tmp/pallium-demo && clear"
Enter
Sleep 500ms
Show

Type 'pallium route "review my changes before I merge"'
Sleep 400ms
Enter
Sleep 5s

Type "clear"
Enter
Type "pallium risk internal/workflow/gate.go"
Sleep 400ms
Enter
Sleep 3500ms
Enter

Type "pallium neighbors internal/workflow/gate.go"
Sleep 400ms
Enter
Sleep 5s
