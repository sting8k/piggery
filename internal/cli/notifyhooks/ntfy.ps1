# written by piggery setup notify add ntfy:{{TOPIC}}
# A push through ntfy.sh to the topic below for each piggery notice. The daemon runs this file with
# Windows PowerShell (powershell.exe), which every Windows has; nothing else is needed.
# One JSON line arrives on stdin (id, from_label, kind, team, gate, dir, body, created_at). It is read
# as UTF-8 bytes (the console's code page would garble it). The body is sent as the request's bytes
# and the title as one encoded header word (RFC 2047, which ntfy reads), never inside a command
# string.
# Plain ASCII on purpose: Windows PowerShell reads a file without a BOM as ANSI.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$buf = New-Object System.IO.MemoryStream
[Console]::OpenStandardInput().CopyTo($buf)
$utf8 = [System.Text.Encoding]::UTF8
$n = $utf8.GetString($buf.ToArray()) | ConvertFrom-Json
$title = 'piggery: ' + $(if ($n.team) { $n.team } elseif ($n.gate) { $n.gate } else { 'notice' })
$title = $title -replace '[\r\n]', '' # as the header's value was one line before it was encoded
$header = '=?UTF-8?B?' + [Convert]::ToBase64String($utf8.GetBytes($title)) + '?='
$body = $utf8.GetBytes([string]$n.body)
# Windows PowerShell on an older Windows does not offer TLS 1.2 by itself.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
[void](Invoke-RestMethod -UseBasicParsing -Method Post -TimeoutSec 8 -Uri 'https://ntfy.sh/{{TOPIC}}' -Headers @{ Title = $header } -Body $body)
