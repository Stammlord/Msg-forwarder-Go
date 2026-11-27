# Карта проекта Msg-forwarder-Go

Это Go-программа-мост: VK и MAX → Telegram / Bark. Один модуль `Msg-forwarder-Go` (Go 1.22, без внешних зависимостей), весь код в пакете `main` в корне.

## Где что лежит

| Файл | Назначение |
|---|---|
| `main.go` | точка входа, флаги `-config`, `-ui-token`, `-test`, `-ctl` |
| `config.go` | `type Config`, загрузка/сохранение `config.json`, значения по умолчанию |
| `config.example.json` | пример конфига с описанием полей |
| `ui.go` | встроенный веб-интерфейс: HTML генерируется прямо в Go (`pageField`, `pageSecret`, `handleIndex`), разбор формы — `configFromForm`, сохранение — `handleSave` |
| `luci/view/Msg-forwarder-Go/index.htm` | вкладка LuCI, форма настроек (расширение **.htm**, не .html) |
| `luci/controller/Msg-forwarder-Go.lua` | контроллер LuCI, читает и пишет те же поля конфига |
| `control.go` | локальный клиент `-ctl status/control/send` для панели соединений |
| `telegram.go` | Telegram Bot API (`https://api.telegram.org/...`) |
| `proxy.go` | HTTP-клиент только для Telegram: `http://`, `https://`, `socks5://`, `socks5h://` |
| `vk.go`, `max.go`, `websocket.go`, `msgpack.go`, `lz4.go`, `codec_test.go` | VK, MAX, протокол WebSocket MAX |
| `bark.go` | push-уведомления Bark |
| `cli.go`, `util.go` | вспомогательные функции, общий `checkRedirect` и лимиты кэша |
| `fileopen_linux.go`, `fileopen_other.go` | открытие файлов лога без следования симлинкам (_linux_ — через `O_NOFOLLOW`) |
| `audit_test.go`, `bench_test.go`, `review_test.go` и др. | тесты |
| `init.d-Msg-forwarder-Go` | procd-сервис OpenWrt |
| `README.md` | документация; раздел «Прокси Telegram» — авторитетный |
| `Msg-forwarder-Go-arm64` | готовый бинарник для роутера (только arm64) |

## Важные факты

- Любая правка формы настроек делается **в трёх местах**: `ui.go` (встроенный UI), `luci/view/Msg-forwarder-Go/index.htm` + `luci/controller/Msg-forwarder-Go.lua` (LuCI), плюс поле в `config.go` и `config.example.json`. Недостаточно изменить только конфиг.
- **MTProto/MTProxy для Bot API не подходит**: программа ходит в `api.telegram.org` по HTTPS, а MTProxy туннелирует только MTProto-трафик клиента Telegram. Официальной поддержки MTProxy для Bot API нет. Поддерживаются только общие HTTP/SOCKS5-прокси — см. `proxy.go` и README.
- Прокси применяется **только** к Telegram. VK, MAX и Bark намеренно не используют `http.ProxyFromEnvironment` и переменные окружения.
- Токены и содержимое сообщений не логируются. Не выводи значения секретов из `config.json` в ответ. Строки от внешних серверов маскируются по секретам до записи и в syslog, и в UI (`debugLog.redact` / `safeDetail`).
- Конфиг сохраняется атомарно с резервной копией `config.json.bak` (0600). Встроенный UI и LuCI сверяют скрытое поле `cfg_mtime`, чтобы не потерять изменения соседнего процесса.
- HTTP-клиенты не следуют редиректам на другой хост/порт (`checkRedirect`), `ws://` для MAX разрешён только на localhost; тяжёлые эндпоинты UI ограничены двумя параллельными операциями.
- Сборка: `go test ./...`, форматирование `gofmt -w *.go`. Бинарник для роутера — `GOOS=linux GOARCH=arm64`; 32-битный ARM не используется.

## Правила работы

- Отвечай по-русски, коротко и по делу. Не переходи на английский.
- Сначала прочитай конкретный файл, потом меняй его. Не делай выводов о проекте только по результатам `glob`/`grep`: оболочка может быть в `.go` или `.htm`, а не в `.html`/`.js`.
- Для правок используй инструменты `write`/`edit`, а не `shell`.
- Не создавай в корне проекта временные файлы, бэкапы и копии. Не переименовывай и не удаляй файлы без явной просьбы.
- Если задача требует изменения сразу нескольких файлов, перечисли их до начала работы и уточни неясности, вместо того чтобы предлагать «создать проект с нуля».
