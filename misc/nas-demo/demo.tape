# vhs tape for the NAS demo.
#   BOX_LOGS=... NAS_CODE=... KD=... KD_SOCKET=... MOUNT=... ./make-gif.sh
#
# The box's container log on the left, the laptop on the right.

Output nas-demo-raw.gif

Set Shell "bash"
Set FontSize 13
Set Width 1500
Set Height 760
Set Padding 14
Set Theme "Catppuccin Mocha"
Set Framerate 12

Hide
Type "clear"
Enter
Sleep 500ms
Show

Type "./nas-demo.sh"
Enter

# Pairing, the listing, one file's hash, the closing line.
Sleep 26s
