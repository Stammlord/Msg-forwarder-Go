#!/bin/sh
# Полное удаление Msg-forwarder-Go с роутера
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

echo "Удаляю файлы..."
rm -f /usr/bin/Msg-forwarder-Go
rm -f /etc/init.d/Msg-forwarder-Go
rm -f /etc/rc.d/*Msg-forwarder-Go 2>/dev/null || true
rm -f /usr/lib/lua/luci/controller/Msg-forwarder-Go.lua
rm -f /usr/lib/lua/luci/view/Msg-forwarder-Go/index.htm
rmdir /usr/lib/lua/luci/view/Msg-forwarder-Go 2>/dev/null || true

if [ "$1" = "--purge" ]; then
  echo "Удаляю конфиг..."
  rm -rf /etc/Msg-forwarder-Go
else
  echo "Конфиг оставлен: /etc/Msg-forwarder-Go/config.json (удали вручную при желании)"
fi

/etc/init.d/uhttpd restart >/dev/null 2>&1 || true
echo "Готово."
