module("luci.controller.Msg-forwarder-Go", package.seeall)

local http       = require("luci.http")
local sys        = require("luci.sys")
local template   = require("luci.template")
local dispatcher = require("luci.dispatcher")

local CONFIG = "/etc/Msg-forwarder-Go/config.json"
local BIN    = "/usr/bin/Msg-forwarder-Go"

local okjson, jsonc = pcall(require, "luci.jsonc")
if not okjson then jsonc = nil end

local STRING_FIELDS = {
	"vk_token", "vk_app_id", "vk_api_version", "tg_bot_token", "telegram_proxy",
	"max_token", "max_device_id", "max_ws_url", "max_user_agent", "max_app_version",
	"bark_server_url", "bark_device_key", "bark_key", "bark_title", "bark_sound",
	"bark_group", "bark_level", "bark_icon", "bark_click_url", "ui_listen",
}

local BOOL_FIELDS = {
	"bark_encrypt", "ui_enabled",
	"autostart_vk", "autostart_max", "autostart_tg", "autostart_bark",
}

local URL_FIELDS = {
	telegram_proxy = {http = true, https = true, socks5 = true, socks5h = true},
	max_ws_url     = {ws = true, wss = true},
	bark_server_url = {http = true, https = true},
}

local CONTROL_SERVICES = {vk = true, max = true, tg = true, bark = true}
local CONTROL_ACTIONS = {
	on = true, off = true, restart = true, autostart = true,
	ping = true, test = true, chatid = true,
}
local SEND_SERVICES = {tg = true, bark = true}

-- Мутирующие цели помечаем как POST; на старых сборках остаётся call().
local function post_or_call(name)
	if type(dispatcher.post) == "function" then
		return dispatcher.post(name)
	end
	return dispatcher.call(name)
end

function index()
	local e = entry({"admin", "services", "Msg-forwarder-Go"}, call("action_index"), _("Msg-forwarder-Go"), 90)
	e.dependent = true
	entry({"admin", "services", "Msg-forwarder-Go", "test"}, post_or_call("action_test"), nil)
	entry({"admin", "services", "Msg-forwarder-Go", "status"}, call("action_status"), nil)
	entry({"admin", "services", "Msg-forwarder-Go", "control"}, post_or_call("action_control"), nil)
	entry({"admin", "services", "Msg-forwarder-Go", "send"}, post_or_call("action_send"), nil)
	entry({"admin", "services", "Msg-forwarder-Go", "debug_status"}, call("action_debug_status"), nil)
	entry({"admin", "services", "Msg-forwarder-Go", "debug"}, post_or_call("action_debug"), nil)
	entry({"admin", "services", "Msg-forwarder-Go", "debug_log"}, call("action_debug_log"), nil)
	entry({"admin", "services", "Msg-forwarder-Go", "rawsave"}, post_or_call("action_raw_save"), nil)
	entry({"admin", "services", "Msg-forwarder-Go", "restart"}, post_or_call("action_restart"), nil)
end

local function val(name)
	return http.formvalue(name) or ""
end

local function vordef(v, def)
	if v == "" then return def end
	return v
end

local function isset(v)
	return v ~= nil and v ~= ""
end

local function defbool(v)
	return v == nil or v == true or v == "true"
end

local function shq(s)
	return "'" .. tostring(s):gsub("'", "'\\''") .. "'"
end

-- Токен сессии LuCI доступен шаблону автоматически; явно берём его для поля формы.
local function csrf_token()
	local ok, token = pcall(function()
		return dispatcher.context and dispatcher.context.authtoken
	end)
	if ok and type(token) == "string" and token ~= "" then return token end
	return ""
end

-- Проверка метода и токена для всех действий, меняющих состояние.
local function check_post()
	local method = ""
	if type(http.getenv) == "function" then
		method = http.getenv("REQUEST_METHOD") or ""
	else
		method = os.getenv("REQUEST_METHOD") or ""
	end
	if method ~= "POST" then
		return "Для этого действия нужен метод POST.", 405
	end
	local expected = csrf_token()
	-- Fail closed: without a CSRF token we cannot protect mutations at all.
	if expected == "" then
		return "Не удалось получить CSRF-токен формы. Перезагрузите страницу.", 403
	end
	if val("token") ~= expected then
		return "Неверный токен формы. Перезагрузите страницу.", 403
	end
	return nil
end

local function post_error(message, code)
	http.status(code or 403, code == 405 and "Метод не разрешён" or "Доступ запрещён")
	if code == 405 and type(http.header) == "function" then
		http.header("Allow", "POST")
	end
	http.prepare_content("text/plain; charset=utf-8")
	http.write(message)
end

local function require_post_action()
	local message, code = check_post()
	if message then
		post_error(message, code)
		return false
	end
	return true
end

-- Небольшой клиент -ctl читает ui.token сам и обращается к локальному API управления.
-- curl на роутере не нужен.
local function ctl_exec(args)
	return sys.exec(BIN .. " -config " .. shq(CONFIG) .. " -ctl " .. args .. " 2>/dev/null") or ""
end

local function read_config()
	if not jsonc then return {} end
	local f = io.open(CONFIG, "r")
	if not f then return {} end
	local s = f:read("*a")
	f:close()
	if not s or s == "" then return {} end
	local ok, c = pcall(jsonc.parse, s)
	if ok and type(c) == "table" then return c end
	return {}
end

-- Редактор JSON показывает файл как есть, включая секреты.
local function read_config_raw()
	local f = io.open(CONFIG, "r")
	if not f then
		if jsonc then
			local ok, s = pcall(jsonc.stringify, {}, true)
			if ok and type(s) == "string" then return s end
		end
		return "{}"
	end
	local s = f:read("*a")
	f:close()
	return s or "{}"
end

local function check_string(c, name)
	if c[name] ~= nil and type(c[name]) ~= "string" then
		return "поле " .. name .. " должно быть строкой"
	end
	return nil
end

local function check_bool(c, name)
	if c[name] ~= nil and type(c[name]) ~= "boolean" then
		return "поле " .. name .. " должно быть логическим"
	end
	return nil
end

-- Разбор host:port без списка разрешённых хостов: свои адреса допустимы.
local function check_hostport(value, name)
	if value == nil or value == "" then return nil end
	if type(value) ~= "string" then
		return "поле " .. name .. " должно быть строкой"
	end

	local port
	if value:sub(1, 1) == "[" then
		local bracket = value:match("^(%[[^%]]+%])")
		if not bracket then return "неверный адрес в поле " .. name end
		local tail = value:sub(#bracket + 1)
		if tail == "" then return "нужен порт 1..65535 в поле " .. name end
		port = tail:match("^:(%d+)$")
		if not port then return "неверный порт в поле " .. name end
	elseif value:sub(1, 1) == ":" then
		port = value:match("^:(%d+)$")
		if not port then return "неверный порт в поле " .. name end
	else
		local _, found = value:match("^([^:]+):(%d+)$")
		port = found
		if not port then
			return "неверный адрес в поле " .. name .. " (нужен host:port)"
		end
	end

	local n = tonumber(port)
	if not n or n < 1 or n > 65535 or n ~= math.floor(n) then
		return "неверный порт в поле " .. name
	end
	return nil
end

-- Разрешаем только явно неверные типы, схемы и порты; hostname остаётся произвольным.
local function check_url(value, name, allowed)
	if value == nil or value == "" then return nil end
	if type(value) ~= "string" then
		return "поле " .. name .. " должно быть строкой"
	end

	local scheme, rest = value:match("^([%a][%w%+%-%.]*)://(.+)$")
	if not scheme or not rest or rest == "" then
		return "неверный адрес в поле " .. name
	end
	scheme = scheme:lower()
	if not allowed[scheme] then
		return "недопустимая схема в поле " .. name
	end

	local authority = rest:match("^([^/?#]+)")
	if not authority or authority == "" then
		return "неверный адрес в поле " .. name
	end
	local hostport = authority:match("@([^@]+)$") or authority
	if hostport:sub(1, 1) == "[" then
		local bracket = hostport:match("^(%[[^%]]+%])")
		if not bracket then return "неверный адрес в поле " .. name end
		local tail = hostport:sub(#bracket + 1)
		if tail ~= "" then
			local port = tail:match("^:(%d+)$")
			if not port then return "неверный порт в поле " .. name end
			local n = tonumber(port)
			if not n or n < 1 or n > 65535 then
				return "неверный порт в поле " .. name
			end
		end
	else
		local _, port = hostport:match("^([^:]+):(%d+)$")
		if port then
			local n = tonumber(port)
			if not n or n < 1 or n > 65535 then
				return "неверный порт в поле " .. name
			end
		elseif hostport:find(":", 1, true) then
			return "неверный адрес или порт в поле " .. name
		end
	end
	return nil
end

local function validate_raw_config(c)
	if type(c) ~= "table" then return "ожидается объект JSON" end

	for _, name in ipairs(STRING_FIELDS) do
		local err = check_string(c, name)
		if err then return err end
	end
	for _, name in ipairs(BOOL_FIELDS) do
		local err = check_bool(c, name)
		if err then return err end
	end
	if c.tg_chat_id ~= nil and type(c.tg_chat_id) ~= "number" and type(c.tg_chat_id) ~= "string" then
		return "поле tg_chat_id должно быть числом или строкой"
	end

	local err = check_hostport(c.ui_listen, "ui_listen")
	if err then return err end

	if c.incoming_opcode ~= nil then
		local n = tonumber(c.incoming_opcode)
		if n == nil or n ~= math.floor(n) or n < 1 or n > 32767 then
			return "неверный incoming_opcode (целое число 1..32767)"
		end
	end

	for name, allowed in pairs(URL_FIELDS) do
		err = check_url(c[name], name, allowed)
		if err then return err end
	end
	-- MAX sends its access token during LOGIN: cleartext ws only for localhost.
	if type(c.max_ws_url) == "string" then
		local scheme, rest = c.max_ws_url:match("^([%a][%w%+%-%.]*)://([^/?#]+)")
		if scheme and scheme:lower() == "ws" then
			local host = rest:match("@([^@]+)$") or rest
			host = host:gsub(":%d+$", ""):gsub("^%[", ""):gsub("%]$", "")
			if host ~= "localhost" and host ~= "::1" and not host:match("^127%.%d+%.%d+%.%d+$") then
				return "max_ws_url: ws без TLS разрешён только для localhost"
			end
		end
	end

	if c.bark_encrypt == true and (c.bark_key == nil or #tostring(c.bark_key) ~= 16) then
		return "bark_key должен быть ровно 16 символов"
	end
	return nil
end

-- Непредсказуемое имя: параллельные сохранения не мешают друг другу, а злоумышленник
-- не может заранее создать симлинк с этим именем.
local function temp_name()
	local seed = ""
	local ok, f = pcall(io.open, "/dev/urandom", "rb")
	if ok and f then
		local b = f:read(16)
		f:close()
		if b and #b == 16 then
			seed = b:gsub(".", function(ch) return string.format("%02x", string.byte(ch)) end)
		end
	end
	if seed == "" then
		seed = tostring(os.time()) .. tostring(os.clock())
		local ok2, nixio = pcall(require, "nixio")
		if ok2 and type(nixio.getpid) == "function" then
			seed = seed .. tostring(nixio.getpid())
		end
		seed = seed:gsub("%W", "")
	end
	return CONFIG .. ".tmp." .. seed
end

-- Создаём файл с O_EXCL и правами 0600 сразу, без окна, когда секреты доступны
-- другим пользователям. nixio есть в luci-base; без него используем umask 077.
local function open_temp(path)
	local ok, nixio = pcall(require, "nixio")
	if ok and type(nixio.open) == "function" and nixio.O_WRONLY and nixio.O_CREAT and nixio.O_EXCL then
		local f, err = nixio.open(path, nixio.O_WRONLY + nixio.O_CREAT + nixio.O_EXCL, tonumber("600", 8))
		if f then return f, nil end
		return nil, err
	end
	if sys.call("umask 077; : > " .. shq(path) .. " 2>/dev/null") ~= 0 then
		return nil, "не удалось создать временный файл"
	end
	return io.open(path, "w")
end

local function write_config(c)
	if not jsonc then return false, "luci.jsonc недоступен — проверьте luci-base" end
	local ok, s = pcall(jsonc.stringify, c, true)
	if not ok or type(s) ~= "string" then
		return false, "не удалось сериализовать конфиг"
	end

	local tmp = temp_name()
	local f, open_err = open_temp(tmp)
	if not f then return false, "нет доступа к " .. CONFIG .. " (" .. tostring(open_err or "ошибка открытия") .. ")" end

	local written = f:write(s .. "\n")
	local closed = f:close()
	if not written or not closed then
		os.remove(tmp)
		return false, "не удалось записать временный файл"
	end

	-- Сохраняем предыдущий конфиг с правами 0600 перед заменой.
	local prev = io.open(CONFIG, "r")
	if prev then
		prev:close()
		sys.call("cp " .. shq(CONFIG) .. " " .. shq(CONFIG .. ".bak") .. " >/dev/null 2>&1")
		sys.call("chmod 600 " .. shq(CONFIG .. ".bak") .. " >/dev/null 2>&1")
	end

	local renamed, rename_err = os.rename(tmp, CONFIG)
	if not renamed then
		os.remove(tmp)
		return false, "не удалось заменить конфиг: " .. tostring(rename_err or "ошибка переименования")
	end

	-- Конфиг остаётся доступным пользователю procd, даже если LuCI сохранял его из-под root.
	sys.call("chown Msg-forwarder-Go:Msg-forwarder-Go " .. shq(CONFIG) .. " >/dev/null 2>&1")
	sys.call("chmod 600 " .. shq(CONFIG) .. " >/dev/null 2>&1")
	return true
end

-- Версия конфига для optimistic locking: LuCI и встроенный UI пишут один файл
-- из разных процессов, поэтому process-local блокировок недостаточно.
local function config_version()
	local ok, nixio = pcall(require, "nixio")
	if ok and type(nixio.stat) == "function" then
		local st = nixio.stat(CONFIG)
		if st and st.mtime then return string.format("%.0f", st.mtime) end
	end
	return (sys.exec("stat -c %Y " .. shq(CONFIG) .. " 2>/dev/null") or ""):gsub("%s+$", "")
end

local function apply_secret(cfg, name, field)
	if http.formvalue("change_" .. name) == "on" then
		cfg[field] = val(name)
	end
	if cfg[field] == nil then cfg[field] = "" end
end

function action_index()
	local cfg = read_config()
	local status, err = nil, nil

	if val("save") == "1" then
		local message, code = check_post()
		if message then
			post_error(message, code)
			return
		end
		local want_version = val("cfg_mtime")
		if want_version ~= "" and config_version() ~= want_version then
			post_error("конфиг был изменён другой сессией — перезагрузите страницу", 409)
			return
		end
		if not jsonc then
			err = "Для сохранения нужен модуль luci.jsonc (часть luci-base)."
		else
			apply_secret(cfg, "vk_token", "vk_token")
			apply_secret(cfg, "tg_bot_token", "tg_bot_token")
			apply_secret(cfg, "max_token", "max_token")
			apply_secret(cfg, "bark_key", "bark_key")

			cfg.vk_app_id      = val("vk_app_id")
			cfg.vk_api_version = vordef(val("vk_api_version"), "5.131")
			cfg.tg_chat_id     = val("tg_chat_id")
			cfg.telegram_proxy = val("telegram_proxy")
			cfg.max_device_id  = val("max_device_id")
			cfg.max_ws_url     = vordef(val("max_ws_url"), "wss://api.oneme.ru/websocket")
			cfg.max_user_agent = vordef(val("max_user_agent"), "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36")
			cfg.max_app_version = vordef(val("max_app_version"), "26.11.9")
			cfg.incoming_opcode = tonumber(val("incoming_opcode")) or 128
			cfg.bark_server_url = vordef(val("bark_server_url"), "https://api.day.app")
			cfg.bark_device_key = val("bark_device_key")
			cfg.bark_title      = vordef(val("bark_title"), "Msg-forwarder-Go")
			cfg.bark_sound      = vordef(val("bark_sound"), "birdsong")
			cfg.bark_group      = vordef(val("bark_group"), "Msg-forwarder-Go")
			cfg.bark_level      = vordef(val("bark_level"), "active")
			cfg.bark_icon       = val("bark_icon")
			cfg.bark_click_url  = val("bark_click_url")
			cfg.bark_encrypt    = http.formvalue("bark_encrypt") == "on"
			cfg.ui_listen      = vordef(val("ui_listen"), "127.0.0.1:8080")
			cfg.ui_enabled     = http.formvalue("ui_enabled") == "on"

			local verr = validate_raw_config(cfg)
			if verr then
				err = verr
			else
				local ok, werr = write_config(cfg)
				if ok then
					status = "Сохранено. Сервис перезапущен."
					sys.call("/etc/init.d/Msg-forwarder-Go restart >/dev/null 2>&1 &")
				else
					err = werr
				end
			end
		end
	end

	local flags = {
		vk = isset(cfg.vk_token),
		tg = isset(cfg.tg_bot_token),
		max = isset(cfg.max_token),
		bark_key = isset(cfg.bark_key),
		oauth = isset(cfg.vk_app_id),
		ui = cfg.ui_enabled ~= false,
	}

	template.render("Msg-forwarder-Go/index", {
		cfg       = cfg,
		flags     = flags,
		status    = status,
		err       = err,
		raw_cfg   = read_config_raw(),
		token     = csrf_token(),
		cfg_mtime = config_version(),
		test_url   = dispatcher.build_url("admin", "services", "Msg-forwarder-Go", "test"),
		status_url = dispatcher.build_url("admin", "services", "Msg-forwarder-Go", "status"),
		control_url = dispatcher.build_url("admin", "services", "Msg-forwarder-Go", "control"),
		send_url   = dispatcher.build_url("admin", "services", "Msg-forwarder-Go", "send"),
		debug_status_url = dispatcher.build_url("admin", "services", "Msg-forwarder-Go", "debug_status"),
		debug_url = dispatcher.build_url("admin", "services", "Msg-forwarder-Go", "debug"),
		debug_log_url = dispatcher.build_url("admin", "services", "Msg-forwarder-Go", "debug_log"),
		rawsave_url = dispatcher.build_url("admin", "services", "Msg-forwarder-Go", "rawsave"),
		restart_url = dispatcher.build_url("admin", "services", "Msg-forwarder-Go", "restart"),
		form_url   = dispatcher.build_url("admin", "services", "Msg-forwarder-Go"),
		autostart  = {
			vk   = defbool(cfg.autostart_vk),
			max  = defbool(cfg.autostart_max),
			tg   = defbool(cfg.autostart_tg),
			bark = defbool(cfg.autostart_bark),
		},
	}
end

-- Отдельная цель для редактора JSON: короткий ответ без перерисовки страницы.
-- Само сохранение не перезапускает сервис.
function action_raw_save()
	if not require_post_action() then return end
	http.prepare_content("text/plain; charset=utf-8")
	if not jsonc then
		http.write("Для сохранения нужен модуль luci.jsonc (часть luci-base).")
		return
	end
	if http.formvalue("raw_edit") ~= "on" then
		http.write("Редактирование не разрешено: поставьте галочку.")
		return
	end

	local ok, c = pcall(jsonc.parse, val("rawconfig"))
	if not ok or type(c) ~= "table" then
		http.write("неверный JSON")
		return
	end
	local verr = validate_raw_config(c)
	if verr then
		http.write(verr)
		return
	end

	local wok, werr = write_config(c)
	if wok then
		http.write("Сохранено. Для применения перезапустите сервис.")
	else
		http.write(werr or "не удалось записать конфиг")
	end
end

function action_test()
	if not require_post_action() then return end
	local mode = val("mode")
	local out

	if mode == "ui-token" then
		-- Показ токена встроенного интерфейса тоже чувствительное действие.
		out = sys.exec(BIN .. " -config " .. CONFIG .. " -ui-token 2>&1")
	elseif mode == "vk" or mode == "tg" or mode == "max" or mode == "bark" or mode == "api" then
		out = sys.exec(BIN .. " -config " .. CONFIG .. " -test " .. mode .. " 2>&1")
	else
		out = "неизвестный режим проверки"
	end

	http.prepare_content("text/plain; charset=utf-8")
	http.write(out or "")
end

function action_status()
	http.prepare_content("application/json; charset=utf-8")
	http.write(ctl_exec("status"))
end

function action_debug_status()
	http.prepare_content("application/json; charset=utf-8")
	http.write(ctl_exec("debug -action status"))
end

function action_debug()
	if not require_post_action() then return end
	local act = val("act")
	if act ~= "on" and act ~= "off" then
		http.prepare_content("text/plain; charset=utf-8")
		http.write("неизвестное действие отладки")
		return
	end
	http.prepare_content("text/plain; charset=utf-8")
	http.write(ctl_exec("debug -action " .. shq(act)))
end

function action_debug_log()
	http.prepare_content("text/plain; charset=utf-8")
	http.header("Cache-Control", "no-store")
	http.header("Content-Disposition", 'attachment; filename="Msg-forwarder-Go-debug.log"')
	http.write(ctl_exec("debug -action log"))
end

function action_control()
	if not require_post_action() then return end
	http.prepare_content("text/plain; charset=utf-8")
	local service = val("service")
	local action = val("action")
	if not CONTROL_SERVICES[service] then
		http.write("неизвестная служба")
		return
	end
	if not CONTROL_ACTIONS[action] then
		http.write("неизвестное действие")
		return
	end

	local args = "control -service " .. shq(service) .. " -action " .. shq(action)
	if action == "autostart" then
		local value = val("value")
		if value ~= "true" and value ~= "false" then
			http.write("неверное значение автозапуска")
			return
		end
		args = args .. " -value " .. shq(value)
	end
	http.write(ctl_exec(args))
end

function action_send()
	if not require_post_action() then return end
	http.prepare_content("text/plain; charset=utf-8")
	local service = val("service")
	if service == "" then service = "tg" end
	if not SEND_SERVICES[service] then
		http.write("неизвестная служба")
		return
	end
	http.write(ctl_exec("send -service " .. shq(service) .. " -text " .. shq(val("text"))))
end

function action_restart()
	if not require_post_action() then return end
	sys.call("/etc/init.d/Msg-forwarder-Go restart >/dev/null 2>&1 &")
	http.prepare_content("text/plain; charset=utf-8")
	http.write("Перезапуск…")
end
