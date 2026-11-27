#!/bin/sh
# Удаление только файлов Msg-forwarder-Go. Системные пакеты и прошивка не трогаются.
# По умолчанию конфиг сохраняется. Для удаления вместе с конфигом: uninstall.sh --purge
set -e

echo "Останавливаю сервис..."
/etc/init.d/Msg-forwarder-Go disable >/dev/null 2>&1 || true
/etc/init.d/Msg-forwarder-Go stop >/dev/null 2>&1 || true
kill $(ps | grep '[M]sg-forwarder-Go' | awk '{print $1}') 2>/dev/null || true

echo "Убираю автозапуск..."
if [ -f /etc/rc.local ]; then
  sed -i '/Msg-forwarder-Go\/start.sh/d' /etc/rc.local 2>/dev/null || true
fi

echo "Удаляю файлы программы..."
rm -f /usr/bin/Msg-forwarder-Go
rm -f /etc/init.d/Msg-forwarder-Go
rm -f /etc/rc.d/*Msg-forwarder-Go 2>/dev/null || true

echo "Удаляю файлы вкладки LuCI..."
rm -f /usr/share/luci/menu.d/luci-app-msgforwarder.json
rm -f /usr/share/rpcd/acl.d/luci-app-msgforwarder.json
rm -rf /www/luci-static/resources/view/msgforwarder

echo "Удаляю старые Lua-файлы, если остались..."
rm -f /usr/lib/lua/luci/controller/Msg-forwarder-Go.lua
rm -f /usr/lib/lua/luci/view/Msg-forwarder-Go/index.htm
rmdir /usr/lib/lua/luci/view/Msg-forwarder-Go 2>/dev/null || true

if [ "$1" = "--purge" ]; then
  echo "Удаляю конфиг..."
  rm -rf /etc/Msg-forwarder-Go
else
  echo "Конфиг оставлен: /etc/Msg-forwarder-Go/config.json"
fi

/etc/init.d/rpcd restart >/dev/null 2>&1 || true
/etc/init.d/uhttpd restart >/dev/null 2>&1 || true
echo "Готово."
