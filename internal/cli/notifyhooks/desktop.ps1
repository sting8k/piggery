# written by piggery setup notify add desktop
# A Windows toast for each piggery notice. The daemon runs this file with Windows PowerShell
# (powershell.exe), which every Windows has; nothing else is needed.
# One JSON line arrives on stdin (id, from_label, kind, team, gate, dir, body, created_at). It is read
# as UTF-8 bytes (the console's code page would garble it), and its fields are set as text nodes,
# never put inside the XML or a command string.
# Plain ASCII on purpose: Windows PowerShell reads a file without a BOM as ANSI.
$ErrorActionPreference = 'Stop'
$buf = New-Object System.IO.MemoryStream
[Console]::OpenStandardInput().CopyTo($buf)
$n = [System.Text.Encoding]::UTF8.GetString($buf.ToArray()) | ConvertFrom-Json
$title = 'piggery: ' + $(if ($n.team) { $n.team } elseif ($n.gate) { $n.gate } else { 'notice' })
$body = [string]$n.body
# XML has no way to write these control characters, escaped or not.
$unwritable = '[\x00-\x08\x0B\x0C\x0E-\x1F]'

$doc = New-Object System.Xml.XmlDocument
$doc.LoadXml('<toast><visual><binding template="ToastGeneric"><text/><text/></binding></visual></toast>')
$texts = $doc.SelectNodes('//text')
[void]$texts.Item(0).AppendChild($doc.CreateTextNode(($title -replace $unwritable, '')))
[void]$texts.Item(1).AppendChild($doc.CreateTextNode(($body -replace $unwritable, '')))

[void][Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]
[void][Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime]
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml($doc.OuterXml)
# A toast is shown under an app Windows knows: Windows PowerShell's own id.
$app = '{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe'
$toast = New-Object Windows.UI.Notifications.ToastNotification $xml
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($app).Show($toast)
