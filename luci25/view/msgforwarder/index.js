'use strict';
'require view';
'require fs';
'require ui';

const CONFIG = '/etc/Msg-forwarder-Go/config.json';
const BIN = '/usr/bin/Msg-forwarder-Go';

const sections = [
	{
		title: _('VK'),
		fields: [
			{ k: 'vk_token', l: _('Токен пользователя'), t: 'secret', h: _('Получить через OAuth ВКонтакте с правами messages или скопировать из настроек своего приложения.') },
			{ k: 'vk_app_id', l: _('ID приложения'), t: 'text', h: _('Создайте приложение в разделе «Разработчикам → Приложения» ВКонтакте и скопируйте его ID.') },
			{ k: 'vk_api_version', l: _('Версия API'), t: 'text', h: _('Версия Long Poll API, например 5.131. Обычно менять не нужно.') }
		]
	},
	{
		title: _('MAX'),
		fields: [
			{ k: 'max_token', l: _('Web-токен'), t: 'secret', h: _('Временный токен веб-версии MAX. Скопируйте из браузера или получите через SMS-авторизацию во встроенном интерфейсе.') },
			{ k: 'max_device_id', l: _('ID устройства'), t: 'secret', h: _('Идентификатор устройства из того же сеанса MAX, что и токен.') },
			{ k: 'max_ws_url', l: _('Адрес WebSocket'), t: 'text', h: _('Адрес подключения MAX. По умолчанию wss://api.oneme.ru/websocket.') },
			{ k: 'max_user_agent', l: _('User-Agent'), t: 'text', h: _('User-Agent браузера, из которого получен токен. Должен совпадать с сеансом MAX.') },
			{ k: 'max_app_version', l: _('Версия клиента'), t: 'text', h: _('Версия веб-клиента MAX, например 26.11.9.') },
			{ k: 'incoming_opcode', l: _('Опкод входящих'), t: 'number', h: _('Код события входящего сообщения. По умолчанию 128; если сообщения не приходят — попробуйте другое значение.') }
		]
	},
	{
		title: _('Telegram'),
		fields: [
			{ k: 'tg_bot_token', l: _('Токен бота'), t: 'secret', h: _('Получите у @BotFather командой /newbot.') },
			{ k: 'tg_chat_id', l: _('ID чата'), t: 'text', h: _('Чат, куда бот шлёт сообщения. Сначала напишите боту /start. Для личной переписки подойдёт ваш ID от @userinfobot; для группы добавьте бота в группу и возьмите chat.id из ответа getUpdates или у @get_id_bot.') },
			{ k: 'telegram_proxy', l: _('Прокси'), t: 'text', h: _('Необязательно. Адрес HTTP/SOCKS5-прокси только для Telegram, например socks5://127.0.0.1:1080.') }
		]
	},
	{
		title: _('Bark'),
		fields: [
			{ k: 'bark_server_url', l: _('Адрес сервера'), t: 'text', h: _('Базовый URL из приложения Bark, например https://api.day.app. Программа сама добавит /push.') },
			{ k: 'bark_device_key', l: _('Ключ устройства'), t: 'secret', h: _('Длинный ключ из ссылки Bark после адреса хоста (в документации пример на 22 символа). Это НЕ ключ шифрования; ограничения по длине нет.') },
			{ k: 'bark_key', l: _('Ключ шифрования'), t: 'secret', h: _('Ровно 16 символов из раздела «Push Encryption → Encryption Settings» приложения Bark. Нужен только при включённом шифровании.') },
			{ k: 'bark_encrypt', l: _('Включить шифрование'), t: 'bool', h: _('Поставьте галочку, если в Bark включено шифрование, и вставьте тот же ключ выше.') },
			{ k: 'bark_title', l: _('Заголовок'), t: 'text', h: _('Поле title в JSON Bark. К нему автоматически добавится источник сообщения.') },
			{ k: 'bark_sound', l: _('Звук'), t: 'text', h: _('Поле sound в JSON Bark, например birdsong.') },
			{ k: 'bark_group', l: _('Группа'), t: 'text', h: _('Поле group в JSON Bark.') },
			{ k: 'bark_level', l: _('Уровень'), t: 'text', h: _('Поле level в JSON Bark: active, timeSensitive, passive или critical.') },
			{ k: 'bark_icon', l: _('Иконка'), t: 'text', h: _('Поле icon в JSON Bark — ссылка на картинку.') },
			{ k: 'bark_click_url', l: _('Ссылка при нажатии'), t: 'text', h: _('Поле url в JSON Bark — адрес, который откроется по нажатию.') }
		]
	},
	{
		title: _('Автозапуск и интерфейс'),
		fields: [
			{ k: 'autostart_vk', l: _('Автозапуск VK'), t: 'bool', h: _('Подключаться к VK автоматически при запуске программы.') },
			{ k: 'autostart_max', l: _('Автозапуск MAX'), t: 'bool', h: _('Подключаться к MAX автоматически при запуске программы.') },
			{ k: 'autostart_tg', l: _('Автозапуск Telegram'), t: 'bool', h: _('Готовить отправку в Telegram автоматически.') },
			{ k: 'autostart_bark', l: _('Автозапуск Bark'), t: 'bool', h: _('Готовить отправку в Bark автоматически.') },
			{ k: 'ui_enabled', l: _('Встроенный интерфейс'), t: 'bool', h: _('Включить локальную панель. По умолчанию выключена, порты не занимаются.') },
			{ k: 'ui_listen', l: _('Адрес прослушивания'), t: 'text', h: _('Адрес и порт встроенной панели, например 127.0.0.1:8080.') }
		]
	}
];

const secretFields = [
	'vk_token', 'tg_bot_token', 'max_token', 'max_device_id',
	'bark_device_key', 'bark_key'
];

function button(label, handler, style) {
	const b = E('button', { 'class': 'btn' + (style ? ' ' + style : ''), 'type': 'button' }, label);
	b.addEventListener('click', handler);
	return b;
}

function fieldInput(field, cfg) {
	let input;

	if (field.t === 'bool') {
		input = E('input', { type: 'checkbox' });
		input.checked = cfg[field.k] !== false;
	}
	else {
		input = E('input', {
			type: (field.t === 'secret') ? 'password' : 'text',
			'data-field': field.k
		});

		if (field.t === 'secret' && cfg[field.k])
			input.placeholder = _('установлено');
		else if (cfg[field.k] !== undefined && cfg[field.k] !== null)
			input.value = String(cfg[field.k]);

		if (field.t === 'number' && cfg[field.k] !== undefined)
			input.value = String(cfg[field.k]);
	}

	return input;
}

function collect(cfg, inputs, types) {
	const out = Object.assign({}, cfg);

	for (const [name, input] of Object.entries(inputs)) {
		if (input.type === 'checkbox') {
			out[name] = input.checked;
			continue;
		}

		const v = input.value.trim();

		if (secretFields.indexOf(name) !== -1 && v === '' && cfg[name])
			continue;

		if (v === '') {
			if (out[name] !== undefined)
				delete out[name];
			continue;
		}

		out[name] = (types[name] === 'number') ? parseInt(v, 10) : v;
	}

	return out;
}

return view.extend({
	load: function () {
		return fs.read(CONFIG).then(function (s) {
			try {
				return JSON.parse(s);
			}
			catch (e) {
				return {};
			}
		}).catch(function () {
			return {};
		});
	},

	render: function (cfg) {
		const inputs = {};
		const types = {};
		const result = E('pre', { 'class': 'mf-result' }, _('Нет вывода'));
		const resultTop = E('pre', { 'class': 'mf-result' }, _('Нет вывода'));

		function show(text) {
			const t = text || _('(пусто)');
			result.textContent = t;
			resultTop.textContent = t;
		}

		function cli(args) {
			return fs.exec(BIN, ['-config', CONFIG].concat(args))
				.then(function (r) {
					return (r && r.stdout) ? r.stdout : (r && r.stderr) ? r.stderr : '';
				})
				.catch(function (e) {
					return _('Ошибка: ') + e.message;
				});
		}

		function ctl(args) {
			return cli(['-ctl'].concat(args));
		}

		function runAndShow(args) {
			show(_('Работаю…'));
			return cli(args).then(show);
		}

		function ctlAndShow(args) {
			show(_('Работаю…'));
			return ctl(args).then(show);
		}

		function downloadLog() {
			show(_('Готовлю лог…'));
			return ctl(['debug', '-action', 'log']).then(function (text) {
				if (text && text.indexOf('FAIL:') === 0) {
					show(text);
					return;
				}
				const blob = new Blob([text], { type: 'text/plain;charset=utf-8' });
				const url = URL.createObjectURL(blob);
				const a = E('a', { href: url, download: 'Msg-forwarder-Go-debug.log' });
				document.body.appendChild(a);
				a.click();
				document.body.removeChild(a);
				URL.revokeObjectURL(url);
				show(_('Лог сохранён в файл Msg-forwarder-Go-debug.log') + ' (' + text.length + ' ' + _('байт') + ')');
			});
		}

		function makeSaveButton() {
			return button(_('Сохранить и перезапустить'), function () {
				const next = collect(cfg, inputs, types);
				fs.write(CONFIG, JSON.stringify(next, null, 2), 0o600)
					.then(function () {
						return fs.exec('/etc/Msg-forwarder-Go/start.sh', []);
					})
					.then(function () {
						show(_('Конфиг сохранён'));
						ui.addNotification(null, E('p', _('Конфиг сохранён')));
					})
					.catch(function (e) {
						show(_('Не удалось сохранить: ') + e.message);
					});
			}, 'btn-action');
		}

		const body = E('div', { 'class': 'cbi-section' });
		body.appendChild(E('div', { 'class': 'cbi-section' }, makeSaveButton()));
		body.appendChild(resultTop);

		body.appendChild(E('h3', _('Подключения')));
		const statusLine = E('div', { 'class': 'mf-status' }, _('Статус не загружен'));
		body.appendChild(statusLine);
		body.appendChild(E('div', { 'class': 'cbi-section' }, [
			button(_('Обновить статус'), function () {
				show(_('Работаю…'));
				ctl(['status']).then(function (s) {
					statusLine.textContent = s;
					show(s);
				});
			}),
			button(_('Проверить VK'), function () { runAndShow(['-test', 'vk']); }),
			button(_('Проверить Bot API'), function () { runAndShow(['-test', 'api']); }),
			button(_('Проверить Telegram'), function () { runAndShow(['-test', 'tg']); }),
			button(_('Проверить MAX'), function () { runAndShow(['-test', 'max']); }),
			button(_('Проверить Bark'), function () { runAndShow(['-test', 'bark']); })
		]));
		body.appendChild(E('p', { 'class': 'mf-help' },
			_('SMS-авторизация MAX доступна только во встроенном интерфейсе: включите его галочкой ниже.')));

		body.appendChild(E('h3', _('Управление сервисами')));
		const controlRow = E('div', { 'class': 'cbi-section' });
		for (const svc of ['vk', 'max', 'tg', 'bark']) {
			controlRow.appendChild(button(svc.toUpperCase() + ': ' + _('вкл'), function () {
				ctlAndShow(['control', '-service', svc, '-action', 'on']);
			}));
			controlRow.appendChild(button(svc.toUpperCase() + ': ' + _('выкл'), function () {
				ctlAndShow(['control', '-service', svc, '-action', 'off']);
			}));
		}
		controlRow.appendChild(button(_('Тест в Telegram'), function () {
			ctlAndShow(['send', '-service', 'tg', '-text', 'проверка']);
		}));
		controlRow.appendChild(button(_('Тест в Bark'), function () {
			ctlAndShow(['send', '-service', 'bark', '-text', 'проверка']);
		}));
		body.appendChild(controlRow);

		for (const sec of sections) {
			body.appendChild(E('h3', sec.title));

			if (sec.title === 'Bark') {
				body.appendChild(E('p', { 'class': 'mf-help' },
					_('Программа отправляет POST /push с JSON. Вкладки GET и POST в приложении Bark — только примеры запроса, заполнять их не нужно. Поле body — это текст самого сообщения, оно подставляется автоматически.')));
			}

			for (const field of sec.fields) {
				const input = fieldInput(field, cfg);
				inputs[field.k] = input;
				types[field.k] = field.t;

				const holder = E('div', { 'class': 'mf-field' }, [
					input,
					E('div', { 'class': 'mf-help' }, field.h)
				]);

				body.appendChild(E('div', { 'class': 'cbi-row' }, [
					E('label', { 'for': 'field-' + field.k }, field.l),
					holder
				]));
				input.id = 'field-' + field.k;
			}
		}

		body.appendChild(E('h3', _('Отладка')));
		body.appendChild(E('div', { 'class': 'cbi-section' }, [
			button(_('Статус отладки'), function () {
				ctlAndShow(['debug', '-action', 'status']);
			}),
			button(_('Включить'), function () {
				ctlAndShow(['debug', '-action', 'on']);
			}),
			button(_('Выключить'), function () {
				ctlAndShow(['debug', '-action', 'off']);
			}),
			button(_('Скачать лог'), function () {
				downloadLog();
			})
		]));

		body.appendChild(E('h3', _('Редактор JSON')));
		const raw = E('textarea', { 'class': 'mf-raw', rows: 8 });
		raw.value = JSON.stringify(cfg, null, 2);
		const jsonBox = E('div', { 'class': 'mf-json' }, [
			raw,
			E('p', { 'class': 'mf-help' }, _('Здесь секреты видны открыто — не показывайте страницу посторонним.')),
			E('div', { 'class': 'cbi-section' }, [
				button(_('Сохранить JSON'), function () {
					let next;
					try {
						next = JSON.parse(raw.value);
					}
					catch (e) {
						show(_('Неверный JSON: ') + e.message);
						return;
					}
					fs.write(CONFIG, JSON.stringify(next, null, 2), 0o600).then(function () {
						return fs.exec('/etc/Msg-forwarder-Go/start.sh', []);
					}).then(function () {
						show(_('Сохранено'));
					}).catch(function (e) {
						show(_('Ошибка: ') + e.message);
					});
				})
			])
		]);
		jsonBox.style.display = 'none';
		body.appendChild(E('div', { 'class': 'cbi-section' }, [
			button(_('Показать редактор JSON'), function () {
				const open = jsonBox.style.display === 'none';
				jsonBox.style.display = open ? '' : 'none';
				this.textContent = open ? _('Скрыть редактор JSON') : _('Показать редактор JSON');
			})
		]));
		body.appendChild(jsonBox);

		body.appendChild(E('div', { 'class': 'cbi-section' }, makeSaveButton()));
		body.appendChild(E('p', {}, [
			_('Токен встроенного интерфейса:'),
			' ',
			E('code', {}, BIN + ' -config ' + CONFIG + ' -ui-token')
		]));
		body.appendChild(result);

		const style = E('style', {},
			'.mf-result { background:#f6f6f6; border:1px solid #ddd; padding:8px; min-height:2em; white-space:pre-wrap; }' +
			'.mf-raw { width:100%; font-family:monospace; }' +
			'.mf-status { background:#f6f6f6; border:1px solid #ddd; padding:8px; white-space:pre-wrap; }' +
			'.cbi-row { display:flex; align-items:flex-start; margin:8px 0; gap:8px; }' +
			'.cbi-row > label { width:230px; flex:0 0 230px; padding-top:5px; }' +
			'.cbi-row .mf-field { flex:1; min-width:0; }' +
			'.cbi-row input[type=text], .cbi-row input[type=password] { width:100%; box-sizing:border-box; }' +
			'.mf-help { color:#777; font-size:12px; margin-top:3px; }' +
			'.btn-action { margin-top:8px; }');

		return E('div', {}, [
			style,
			E('h2', _('Msg-forwarder-Go')),
			body
		]);
	}
});
