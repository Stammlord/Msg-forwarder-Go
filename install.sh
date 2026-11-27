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

/etc/init.d/Msg-forwarder-Go enable
/etc/init.d/Msg-forwarder-Go restart

echo "Готово. Токен веб-интерфейса:"
echo "  /usr/bin/Msg-forwarder-Go -config /etc/Msg-forwarder-Go/config.json -ui-token"
