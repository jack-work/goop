# Acquires an AAD access token via the Windows WAM broker (MSAL.NET) using the
# MSAL assemblies that ship with the Az.Accounts PowerShell module. Outputs a
# JSON object: {"accessToken": "...", "expiresOn": "<ISO8601>"}.
#
# This is the same broker-based silent-token pattern used by tomb; the only
# difference is the ClientId / Scope are parameters so it can be shared.
param(
  [Parameter(Mandatory = $true)][string]$ClientId,
  [Parameter(Mandatory = $true)][string]$Scope,
  [string]$TenantId = "72f988bf-86f1-41af-91ab-2d7cd011db47"
)
$ErrorActionPreference = "Stop"

# Locate the newest installed Az.Accounts module (ships the MSAL assemblies).
$azRoot = "$env:USERPROFILE\.local\share\powershell\Modules\Az.Accounts"
if (-not (Test-Path $azRoot)) {
  $azRoot = (Get-Module -ListAvailable Az.Accounts | Select-Object -First 1).ModuleBase | Split-Path
}
$verDir = Get-ChildItem $azRoot -Directory |
  Sort-Object { [version]($_.Name) } -Descending |
  Select-Object -First 1
$azModPath = Join-Path $verDir.FullName "lib\netstandard2.0"

Get-ChildItem $verDir.FullName -Recurse -Filter "Microsoft.IdentityModel.Abstractions.dll" |
  Select-Object -First 1 |
  ForEach-Object { Add-Type -Path $_.FullName }
Add-Type -Path "$azModPath\Microsoft.Identity.Client.dll"
Add-Type -Path "$azModPath\Microsoft.Identity.Client.Broker.dll"

$appBuilder = [Microsoft.Identity.Client.PublicClientApplicationBuilder]::Create($ClientId)
$appBuilder = $appBuilder.WithAuthority("https://login.microsoftonline.com/$TenantId")
[Microsoft.Identity.Client.Broker.BrokerExtension]::WithBroker(
  $appBuilder,
  (New-Object Microsoft.Identity.Client.BrokerOptions([Microsoft.Identity.Client.BrokerOptions+OperatingSystems]::Windows))
) | Out-Null
$app = $appBuilder.Build()

$result = $app.AcquireTokenSilent(
  [string[]]@($Scope),
  [Microsoft.Identity.Client.PublicClientApplication]::OperatingSystemAccount
).ExecuteAsync().GetAwaiter().GetResult()

[pscustomobject]@{
  accessToken = $result.AccessToken
  expiresOn   = $result.ExpiresOn.UtcDateTime.ToString("o")
} | ConvertTo-Json -Compress
