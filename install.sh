#!/bin/sh
# Установка Msg-forwarder-Go на OpenWrt (arm64) одной командой
set -e

BASE=https://raw.githubusercontent.com/Stammlord/Msg-forwarder-Go/main

if [ "$(uname -m)" != "aarch64" ]; then
  echo "Нужна архитектура aarch64 (arm64), а не $(uname -m)" >&2
  exit 1
fi

echo "Скачиваю бинарник..."
wget -qO /usr/bin/Msg-forwarder-Go "$BASE/Msg-forwarder-Go-arm64"
SZ=$(wc -c < /usr/bin/Msg-forwarder-Go)
if [ "$SZ" -lt 5000000 ]; then
  echo "Бинарник скачался не полностью: $SZ байт" >&2
  exit 1
fi
chmod 755 /usr/bin/Msg-forwarder-Go

echo "Скачиваю сервис..."
wget -qO /etc/init.d/Msg-forwarder-Go "$BASE/init.d-Msg-forwarder-Go"
chmod 755 /etc/init.d/Msg-forwarder-Go

mkdir -p /etc/Msg-forwarder-Go
[ -f /etc/Msg-forwarder-Go/config.json ] || echo '{}' > /etc/Msg-forwarder-Go/config.json
chmod 700 /etc/Msg-forwarder-Go
chmod 600 /etc/Msg-forwarder-Go/config.json

/etc/init.d/Msg-forwarder-Go enable >/dev/null 2>&1 || true
/etc/init.d/Msg-forwarder-Go restart >/dev/null 2>&1 || true

if ! ps | grep -v grep | grep -q '[M]sg-forwarder-Go'; then
  echo "procd не поднял сервис — запускаю через rc.local"
  /etc/init.d/Msg-forwarder-Go disable >/dev/null 2>&1 || true
  cat > /etc/Msg-forwarder-Go/start.sh <<'EOF'
#!/bin/sh
/usr/bin/Msg-forwarder-Go -config /etc/Msg-forwarder-Go/config.json >/dev/null 2>&1 &
EOF
  chmod 755 /etc/Msg-forwarder-Go/start.sh
  LINE='/etc/Msg-forwarder-Go/start.sh'
  [ -f /etc/rc.local ] || { echo '#!/bin/sh' > /etc/rc.local; echo 'exit 0' >> /etc/rc.local; chmod 755 /etc/rc.local; }
  if ! grep -qF "$LINE" /etc/rc.local; then
    if grep -q '^exit 0' /etc/rc.local; then
      sed -i "s|^exit 0|$LINE\nexit 0|" /etc/rc.local
    else
      echo "$LINE" >> /etc/rc.local
    fi
  fi
  /etc/Msg-forwarder-Go/start.sh
fi

echo "Ставлю вкладку LuCI..."
mkdir -p /usr/lib/lua/luci/controller /usr/lib/lua/luci/view/Msg-forwarder-Go
wget -qO /usr/lib/lua/luci/controller/Msg-forwarder-Go.lua "$BASE/luci/controller/Msg-forwarder-Go.lua"
wget -qO /usr/lib/lua/luci/view/Msg-forwarder-Go/index.htm "$BASE/luci/view/Msg-forwarder-Go/index.htm"
/etc/init.d/uhttpd restart >/dev/null 2>&1 || true

echo "Готово. Интерфейс: LuCI → Службы → Msg-forwarder-Go"
echo "Токен встроенного веб-интерфейса:"
echo "  /usr/bin/Msg-forwarder-Go -config /etc/Msg-forwarder-Go/config.json -ui-token"
