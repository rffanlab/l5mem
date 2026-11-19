#!/usr/bin/env bash
# 在 158 上以 root 运行：sudo bash deploy-l5mem.sh <release.tar.gz>
# 幂等；失败回滚到旧版本；不动数据（/srv/e5-data/l5mem）。
set -Eeuo pipefail
TARBALL=${1:?release tar.gz}
ID=l5mem; APP=/srv/e5-apps/$ID; DATA=/srv/e5-data/$ID; LOG=/srv/e5-logs/$ID; ENVF=/etc/e5-runtime/$ID.env
VER=$(date +%Y%m%d-%H%M%S); REL=$APP/releases/$VER
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
PREV=$(readlink -f $APP/current 2>/dev/null || true)

echo "== 1/7 检查端口"
for p in 9110 19110; do
  if ss -ltn "sport = :$p" | grep -q LISTEN && ! systemctl is-active -q $ID.service; then
    echo "端口 $p 已被别的程序占用：$(ss -ltnp "sport = :$p" | tail -n +2)"; exit 1; fi
done

echo "== 2/7 用户与目录"
id $ID >/dev/null 2>&1 || useradd --system --home-dir $DATA --shell /usr/sbin/nologin $ID
install -d -o $ID -g $ID -m 0750 $DATA $DATA/logs $LOG
install -d -o root -g root -m 0755 $APP $APP/releases /etc/e5-runtime

echo "== 3/7 安装版本 $VER"
mkdir -p $REL && tar -xzf "$TARBALL" -C $REL && touch $REL
chown -R root:root $REL && chmod 0755 $REL/l5
$REL/l5 version
install -o root -g root -m 0755 $REL/l5 /usr/local/bin/l5
ln -sfn /usr/local/bin/l5 /usr/local/bin/memo

echo "== 4/7 环境文件（root 0600）"
if [ ! -f $ENVF ]; then
  TOKEN=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
  printf 'L5_TOKEN=%s\nL5_WATCH_AT=10:17\nL5_NOTIFY_URL=\nTZ=Asia/Shanghai\n' "$TOKEN" > $ENVF
  chmod 0600 $ENVF
  echo "已创建 $ENVF"
  echo "【写入令牌】L5_TOKEN=$TOKEN   （远程机器人写入时要带；本机走 socket 不需要）"
else
  echo "已存在，保留原值（令牌见 $ENVF）"
fi

echo "== 5/7 systemd 并启动"
install -o root -g root -m 0644 $REL/deploy/l5mem.service /etc/systemd/system/l5mem.service
systemctl daemon-reload
ln -sfn $REL $APP/current
rollback() {
  echo "!! 失败，回滚"; systemctl stop $ID || true
  [ -n "$PREV" ] && ln -sfn "$PREV" $APP/current && systemctl start $ID || true
  exit 1; }
systemctl enable $ID.service >/dev/null
systemctl restart $ID.service || { journalctl -u $ID -n 30 --no-pager; tail -n 30 $LOG/l5mem.log; rollback; }
for i in $(seq 1 30); do curl -fsS --max-time 2 http://127.0.0.1:19110/healthz >/dev/null 2>&1 && break; sleep 1; [ $i = 30 ] && { tail -n 30 $LOG/l5mem.log; rollback; }; done
# 同义词组（英伟达 / NVIDIA / NVDA ……）：每次部署都载入一遍，重复载入安全
[ -f $REL/deploy/aliases.txt ] && L5_CALLER=deploy /usr/local/bin/l5 alias load $REL/deploy/aliases.txt || true
# 本机业务走 socket：InfoCollector 的运行用户加进 l5mem 组
if id infocollector >/dev/null 2>&1 && ! id -nG infocollector | grep -qw $ID; then
  usermod -aG $ID infocollector && echo "已把 infocollector 加进 $ID 组（重启 InfoCollector 后生效）"
  systemctl try-restart infocollector-worker.service infocollector-web.service || true
fi
# 本机的命令行：rffanlab 也加进组，memo 直接可用
id rffanlab >/dev/null 2>&1 && usermod -aG $ID rffanlab || true

echo "== 6/7 Nginx 网关"
install -o root -g root -m 0644 $REL/deploy/nginx-l5mem.conf /etc/nginx/sites-available/e5-l5mem.conf
ln -sfn /etc/nginx/sites-available/e5-l5mem.conf /etc/nginx/sites-enabled/e5-l5mem.conf
nginx -t && systemctl reload nginx.service

echo "== 7/7 注册到 E5 Manager"
code=$(curl -sS -o $WORK/reg.out -w '%{http_code}' -X POST http://127.0.0.1:18090/api/registry/services \
  -H 'Content-Type: application/json' -H 'X-E5-Confirm: E5-CONFIRMED' --data-binary @$REL/deploy/service-registration.json || true)
echo "注册返回 HTTP $code"; cat $WORK/reg.out; echo
case $code in 2??|409) ;; *) echo "[!] 注册没成功（服务本身已经在跑；E5 的要求改好后重跑本脚本即可）";; esac
CUR=$(readlink -f $APP/current)
ls -1d $APP/releases/*/ 2>/dev/null | sed 's#/$##' | sort | head -n -3 | while read -r old; do
  [ "$(readlink -f "$old")" = "$CUR" ] || rm -rf "$old"
done
echo "部署完成：$VER"
