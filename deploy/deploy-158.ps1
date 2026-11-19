# 在你的 Windows 上运行：打包（已编译好的 Linux 程序 bin/l5 + deploy/）-> scp 到 158 -> ssh 执行部署脚本
# 需要：158 免密 ssh、rffanlab 有 sudo。不需要在 Windows 上装 Go。
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Start-Transcript -Path (Join-Path $Root "deploy-158.log") -Force | Out-Null
$Remote = "rffanlab@192.168.1.158"
Set-Location $Root

if (-not (Test-Path "bin\l5")) { throw "缺少 bin\l5（Linux 程序）。在 Linux/WSL 上运行 build.sh 生成。" }
Write-Host "== 检查 ssh 免密"
ssh -o BatchMode=yes -o ConnectTimeout=8 $Remote "echo ok" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "ssh 到 $Remote 失败（需要免密登录）" }

Write-Host "== 打包"
$stage = Join-Path $env:TEMP "l5mem-stage"
if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
New-Item -ItemType Directory -Path $stage | Out-Null
Copy-Item "bin\l5" (Join-Path $stage "l5")
Copy-Item "deploy" (Join-Path $stage "deploy") -Recurse
Copy-Item "clients" (Join-Path $stage "clients") -Recurse
$tgz = Join-Path $env:TEMP "l5mem-release.tgz"
if (Test-Path $tgz) { Remove-Item $tgz }
tar -czf $tgz -C $stage .
if ($LASTEXITCODE -ne 0) { throw "打包失败" }
Remove-Item $stage -Recurse -Force

Write-Host "== 上传"
ssh $Remote "rm -rf /tmp/l5-deploy && mkdir -p /tmp/l5-deploy && chmod 700 /tmp/l5-deploy"
scp $tgz "${Remote}:/tmp/l5-deploy/release.tgz"

Write-Host "== 在 158 上部署（可能要输入 rffanlab 的 sudo 密码）"
$cmd = "cd /tmp/l5-deploy && mkdir -p src && tar -xzf release.tgz -C src && sed -i 's/\r`$//' src/deploy/*.sh && sudo -n true 2>/dev/null || echo '需要 sudo 密码：'; sudo bash src/deploy/deploy-l5mem.sh /tmp/l5-deploy/release.tgz; rc=`$?; bash src/deploy/verify-l5mem.sh; cd /; rm -rf /tmp/l5-deploy; exit `$rc"
ssh -t $Remote $cmd
$rc = $LASTEXITCODE
Remove-Item $tgz -Force -ErrorAction SilentlyContinue
if ($rc -eq 0) {
  Write-Host ""
  Write-Host "部署完成。网页 http://192.168.1.158:9110/   命令行：在 158 上直接用 l5 或 memo（例如 memo search 订阅）" -ForegroundColor Green
  Write-Host "远程写入令牌在 158 的 /etc/e5-runtime/l5mem.env（L5_TOKEN），首次部署时上面也打印了。"
} else { Write-Host "部署失败，见上面的输出" -ForegroundColor Red }
Stop-Transcript | Out-Null
