$ErrorActionPreference = 'Stop'

$toolsDir = "$(Split-Path -Parent $MyInvocation.MyCommand.Definition)"

# Remove from PATH
Get-ChildItem $toolsDir -Directory -Filter "keibidrop-*" | ForEach-Object {
  Uninstall-ChocolateyPath -PathToUninstall $_.FullName -PathType 'Machine'
}

Write-Host "KeibiDrop uninstalled."
