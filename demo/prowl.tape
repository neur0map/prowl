# Prowl demo tape. Render a GIF with the VHS tool (https://github.com/charmbracelet/vhs):
#
#   go install github.com/charmbracelet/vhs@latest   # needs ttyd + ffmpeg on PATH
#   cd <a repo with prowl on PATH>
#   vhs /path/to/demo/prowl.tape
#
# It records `init`, `overview`, `bench`, and `graph` into demo/prowl.gif, running
# in the current directory. Point it at a real project for the best result.

Output demo/prowl.gif
Require prowl

Set Shell bash
Set FontSize 15
Set Width 1200
Set Height 860
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 45ms

Hide
Type "clear" Enter
Show

# Set up the project: one command wires the index, agent config, and skills,
# and prints the ready card.
Type "prowl init --no-input"
Sleep 600ms
Enter
Sleep 5s
# Orient: a compact, cited map of the whole project (also refreshes AGENTS.md).
Type "prowl overview"
Sleep 600ms
Enter
Sleep 4s

# Prove it: cited packets vs reading the files vs the whole repo. No cloud, no keys.
Type "prowl bench"
Sleep 600ms
Enter
Sleep 5s

# See it: a self-contained interactive dependency graph you can open in a browser.
Type "prowl graph"
Sleep 600ms
Enter
Sleep 3s

Sleep 2s
