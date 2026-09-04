# vhs tape for the two-agent demo.
#   KDMCP=/path/to/kdmcp ./make-gif.sh
#
# Two agents, one per pane, each driving its own KeibiDrop peer through MCP.
# The laptop holds a dataset. The gpu box reads it without copying it. The
# clock along the bottom runs for the whole thing.

Output agent-demo-raw.gif

Set Shell "bash"
Set FontSize 13
Set Width 1500
Set Height 860
Set Padding 14
Set Theme "Catppuccin Mocha"
Set Framerate 12

Hide
Type "clear"
Enter
Sleep 500ms
Show

Type "./two-agents.sh"
Enter

# Pairing, listing, the read, and the closing numbers.
Sleep 32s
Sleep 6s
