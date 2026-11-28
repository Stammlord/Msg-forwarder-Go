#!/bin/sh
# Установка Msg-forwarder-Go на OpenWrt 25.12+ одной командой
set -e

BASE=https://raw.githubusercontent.com/Stammlord/Msg-forwarder-Go/main

if [ "$(uname -m)" != "aarch64" ]; then
  echo "Нужна архитектура aarch64 (arm64), а не $(uname -m)" >&2
  exit 1
fi

if [ -f /usr/bin/Msg-forwarder-Go ] && [ "$(wc -c < /usr/bin/Msg-forwarder-Go)" -ge 5000000 ]; then
  echo "Бинарник уже есть ($(wc -c < /usr/bin/Msg-forwarder-Go) байт), пропускаю скачивание"
else
  echo "Скачиваю бинарник..."
  rm -f /usr/bin/Msg-forwarder-Go.new
  wget -O /usr/bin/Msg-forwarder-Go.new "$BASE/Msg-forwarder-Go-arm64"
  SZ=$(wc -c < /usr/bin/Msg-forwarder-Go.new)
  if [ "$SZ" -lt 5000000 ]; then
    echo "Бинарник скачался не полностью: $SZ байт" >&2
    rm -f /usr/bin/Msg-forwarder-Go.new
    exit 1
  fi
  chmod 755 /usr/bin/Msg-forwarder-Go.new
  mv -f /usr/bin/Msg-forwarder-Go.new /usr/bin/Msg-forwarder-Go
fi

echo "Скачиваю сервис..."
wget -qO /etc/init.d/Msg-forwarder-Go "$BASE/init.d-Msg-forwarder-Go"
chmod 755 /etc/init.d/Msg-forwarder-Go

mkdir -p /etc/Msg-forwarder-Go
[ -f /etc/Msg-forwarder-Go/config.json ] || echo '{}' > /etc/Msg-forwarder-Go/config.json
# Настройка идёт из LuCI, поэтому встроенный UI по умолчанию выключен и не занимает порт.
if grep -q '"ui_enabled"' /etc/Msg-forwarder-Go/config.json; then
  sed -i 's/"ui_enabled"[[:space:]]*:[[:space:]]*true/"ui_enabled":false/' /etc/Msg-forwarder-Go/config.json
elif grep -q '^{[[:space:]]*}$' /etc/Msg-forwarder-Go/config.json; then
  echo '{"ui_enabled":false}' > /etc/Msg-forwarder-Go/config.json
else
  sed -i '1s/^{/{"ui_enabled":false,/' /etc/Msg-forwarder-Go/config.json
fi
chmod 700 /etc/Msg-forwarder-Go
chmod 600 /etc/Msg-forwarder-Go/config.json

/etc/init.d/Msg-forwarder-Go enable >/dev/null 2>&1 || true
/etc/init.d/Msg-forwarder-Go restart >/dev/null 2>&1 || true
sleep 2

echo "Создаю служебные скрипты..."
/etc/init.d/Msg-forwarder-Go disable >/dev/null 2>&1 || true
cat > /etc/Msg-forwarder-Go/start.sh <<'EOF'
#!/bin/sh
/etc/init.d/Msg-forwarder-Go disable >/dev/null 2>&1 || true
LINE='/etc/Msg-forwarder-Go/start.sh'
[ -f /etc/rc.local ] || { echo '#!/bin/sh' > /etc/rc.local; echo 'exit 0' >> /etc/rc.local; chmod 755 /etc/rc.local; }
if ! grep -qF "$LINE" /etc/rc.local; then
  if grep -q '^exit 0' /etc/rc.local; then
    sed -i "s|^exit 0|$LINE\nexit 0|" /etc/rc.local
  else
    echo "$LINE" >> /etc/rc.local
  fi
fi
kill $(ps | grep '[M]sg-forwarder-Go' | awk '{print $1}') 2>/dev/null
sleep 1
/usr/bin/Msg-forwarder-Go -config /etc/Msg-forwarder-Go/config.json >/dev/null 2>&1 &
EOF
  cat > /etc/Msg-forwarder-Go/stop.sh <<'EOF'
#!/bin/sh
/etc/init.d/Msg-forwarder-Go disable >/dev/null 2>&1 || true
sed -i '/Msg-forwarder-Go\/start.sh/d' /etc/rc.local 2>/dev/null || true
kill $(ps | grep '[M]sg-forwarder-Go' | awk '{print $1}') 2>/dev/null
sleep 1
EOF
  cat > /etc/Msg-forwarder-Go/autostart.sh <<'EOF'
#!/bin/sh
if grep -qF '/etc/Msg-forwarder-Go/start.sh' /etc/rc.local 2>/dev/null; then
  echo 'автозапуск: включён (rc.local)'
else
  echo 'автозапуск: выключен'
fi
if ps | grep -q '[M]sg-forwarder-Go'; then
  echo 'процесс: запущен'
else
  echo 'процесс: остановлен'
fi
EOF
  cat > /etc/Msg-forwarder-Go/autostart-on.sh <<'EOF'
#!/bin/sh
/etc/init.d/Msg-forwarder-Go disable >/dev/null 2>&1 || true
LINE='/etc/Msg-forwarder-Go/start.sh'
[ -f /etc/rc.local ] || { echo '#!/bin/sh' > /etc/rc.local; echo 'exit 0' >> /etc/rc.local; chmod 755 /etc/rc.local; }
if ! grep -qF "$LINE" /etc/rc.local; then
  if grep -q '^exit 0' /etc/rc.local; then
    sed -i "s|^exit 0|$LINE\nexit 0|" /etc/rc.local
  else
    echo "$LINE" >> /etc/rc.local
  fi
fi
EOF
  cat > /etc/Msg-forwarder-Go/autostart-off.sh <<'EOF'
#!/bin/sh
/etc/init.d/Msg-forwarder-Go disable >/dev/null 2>&1 || true
sed -i '/Msg-forwarder-Go\/start.sh/d' /etc/rc.local 2>/dev/null || true
EOF
  cat > /etc/Msg-forwarder-Go/process-start.sh <<'EOF'
#!/bin/sh
/etc/init.d/Msg-forwarder-Go stop >/dev/null 2>&1 || true
/etc/init.d/Msg-forwarder-Go disable >/dev/null 2>&1 || true
kill $(ps | grep '[M]sg-forwarder-Go' | awk '{print $1}') 2>/dev/null
sleep 1
/usr/bin/Msg-forwarder-Go -config /etc/Msg-forwarder-Go/config.json >/dev/null 2>&1 &
EOF
  cat > /etc/Msg-forwarder-Go/process-stop.sh <<'EOF'
#!/bin/sh
/etc/init.d/Msg-forwarder-Go stop >/dev/null 2>&1 || true
/etc/init.d/Msg-forwarder-Go disable >/dev/null 2>&1 || true
kill $(ps | grep '[M]sg-forwarder-Go' | awk '{print $1}') 2>/dev/null
sleep 1
EOF
chmod 755 /etc/Msg-forwarder-Go/*.sh
if ! ps | grep -v grep | grep -q '[M]sg-forwarder-Go'; then
  echo "procd не поднял сервис — запускаю через rc.local"
  /etc/Msg-forwarder-Go/start.sh
fi

echo "Убираю старые Lua-файлы вкладки..."
rm -f /usr/lib/lua/luci/controller/Msg-forwarder-Go.lua
rm -f /usr/lib/lua/luci/view/Msg-forwarder-Go/index.htm
rmdir /usr/lib/lua/luci/view/Msg-forwarder-Go 2>/dev/null || true

echo "Ставлю вкладку LuCI 25.12+..."
mkdir -p /usr/share/luci/menu.d /usr/share/rpcd/acl.d /www/luci-static/resources/view/msgforwarder
wget -qO /usr/share/luci/menu.d/luci-app-msgforwarder.json "$BASE/luci25/menu.d/luci-app-msgforwarder.json"
wget -qO /usr/share/rpcd/acl.d/luci-app-msgforwarder.json "$BASE/luci25/rpcd/acl.d/luci-app-msgforwarder.json"
wget -qO /www/luci-static/resources/view/msgforwarder/index.js "$BASE/luci25/view/msgforwarder/index.js"
rm -f /tmp/luci-* 2>/dev/null || true
rm -rf /var/luci-modulecache 2>/dev/null || true
rm -f /var/luci-indexcache 2>/dev/null || true
/etc/init.d/rpcd restart >/dev/null 2>&1 || true
/etc/init.d/uhttpd restart >/dev/null 2>&1 || true

echo "Готово. Вкладка: LuCI → Службы → Msg-forwarder-Go"
