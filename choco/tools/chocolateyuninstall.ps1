$ErrorActionPreference = 'Stop'

$toolsDir = "$(Split-Path -Parent $MyInvocation.MyCommand.Definition)"

# Remove from PATH (matches what Install-ChocolateyPath added in install script)
Uninstall-ChocolateyPath -PathToUninstall $toolsDir -PathType 'Machine'

Write-Host "KeibiDrop uninstalled."
