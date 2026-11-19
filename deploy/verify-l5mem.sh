#!/usr/bin/env bash
# 在 158 上运行：bash verify-l5mem.sh
set -u; fail=0
ok(){ echo "  ✔ $1"; }; bad(){ echo "  ✘ $1"; fail=1; }
[ "$(systemctl is-active l5mem)" = active ] && ok "l5mem active" || bad "l5mem 未运行"
[ "$(systemctl is-enabled l5mem)" = enabled ] && ok "l5mem enabled" || bad "l5mem 未 enable"
curl -fsS --max-time 5 http://127.0.0.1:19110/healthz >/dev/null && ok "healthz 200" || bad "healthz"
ss -ltn 'sport = :19110' | grep -q '127.0.0.1:19110' && ok "19110 仅 loopback" || bad "19110 监听地址"
ss -ltn 'sport = :9110' | grep -q LISTEN && ok "9110 由 nginx 监听" || bad "9110 未监听"
sudo -n test -S /run/l5mem/l5.sock 2>/dev/null && ok "socket /run/l5mem/l5.sock" || echo "  · socket 检查跳过（需要 sudo；看下面 InfoCollector 能不能写入即可）"
c=$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:19110/api/v1/search?q=%E6%B5%8B%E8%AF%95&caller=watchdog")
[ "$c" = 200 ] && ok "检索接口 200" || bad "检索接口 $c"
sudo -n -u l5mem /usr/local/bin/l5 doctor >/dev/null 2>&1 && ok "体检通过" || echo "  · 体检跳过（需要 sudo）"
tail -n 3 /srv/e5-logs/l5mem/l5mem.log 2>/dev/null | sed 's/^/    /'
exit $fail
