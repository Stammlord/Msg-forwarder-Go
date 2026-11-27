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
			['vk_token', _('User token'), 'secret'],
			['vk_app_id', _('App ID')],
			['vk_api_version', _('API version')]
		]
	},
	{
		title: _('Telegram'),
		fields: [
			['tg_bot_token', _('Bot token'), 'secret'],
			['tg_chat_id', _('Chat ID')],
			['telegram_proxy', _('Proxy (http:// or socks5://)')]
		]
	},
	{
		title: _('MAX'),
		fields: [
			['max_token', _('Web token'), 'secret'],
			['max_device_id', _('Device ID'), 'secret'],
			['max_ws_url', _('WebSocket URL')],
			['max_user_agent', _('User-Agent')],
			['max_app_version', _('App version')],
			['incoming_opcode', _('Incoming opcode'), 'number']
		]
	},
	{
		title: _('Bark'),
		fields: [
			['bark_server_url', _('Server URL')],
			['bark_device_key', _('Device key'), 'secret'],
			['bark_key', _('Encryption key'), 'secret'],
			['bark_encrypt', _('Enable encryption'), 'bool'],
			['bark_title', _('Title')],
			['bark_sound', _('Sound')],
			['bark_group', _('Group')],
			['bark_level', _('Level')],
			['bark_icon', _('Icon')],
			['bark_click_url', _('Click URL')]
		]
	},
	{
		title: _('Autostart and interface'),
		fields: [
			['autostart_vk', _('VK'), 'bool'],
			['autostart_max', _('MAX'), 'bool'],
			['autostart_tg', _('Telegram'), 'bool'],
			['autostart_bark', _('Bark'), 'bool'],
			['ui_enabled', _('Built-in web interface'), 'bool'],
			['ui_listen', _('Listen address')]
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

function fieldInput(name, type, cfg) {
	let input;

	if (type === 'bool') {
		input = E('input', { type: 'checkbox' });
		input.checked = cfg[name] !== false;
	}
	else {
		input = E('input', {
			type: (type === 'secret') ? 'password' : 'text',
			'data-field': name
		});

		if (type === 'secret' && cfg[name])
			input.placeholder = _('configured');
		else if (cfg[name] !== undefined && cfg[name] !== null)
			input.value = String(cfg[name]);

		if (type === 'number' && cfg[name] !== undefined)
			input.value = String(cfg[name]);
	}

	return input;
}

function collect(cfg, inputs) {
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

		out[name] = (input.dataset.type === 'number') ? parseInt(v, 10) : v;
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
		const result = E('pre', { 'id': 'mf-result', 'class': 'mf-result' }, _('No output yet'));

		function show(text) {
			result.textContent = text || _('(empty)');
		}

		function cli(args) {
			return fs.exec(BIN, ['-config', CONFIG].concat(args))
				.then(function (r) {
					return (r && r.stdout) ? r.stdout : (r && r.stderr) ? r.stderr : '';
				})
				.catch(function (e) {
					return _('Error: ') + e.message;
				});
		}

		function ctl(args) {
			return cli(['-ctl'].concat(args));
		}

		function runAndShow(args) {
			show(_('Working…'));
			return cli(args).then(show);
		}

		function ctlAndShow(args) {
			show(_('Working…'));
			return ctl(args).then(show);
		}

		const body = E('div', { 'class': 'cbi-section' });

		body.appendChild(E('h3', _('Connections')));
		const statusLine = E('div', { 'class': 'mf-status' }, _('Status not loaded'));
		body.appendChild(statusLine);
		body.appendChild(E('div', { 'class': 'cbi-section' }, [
			button(_('Refresh status'), function () {
				ctlAndShow(['status']).then(function () {
					return ctl(['status']);
				}).then(function (s) {
					statusLine.textContent = s;
				});
			}),
			button(_('Check VK'), function () { runAndShow(['-test', 'vk']); }),
			button(_('Check Bot API'), function () { runAndShow(['-test', 'api']); }),
			button(_('Check Telegram'), function () { runAndShow(['-test', 'tg']); }),
			button(_('Check MAX'), function () { runAndShow(['-test', 'max']); }),
			button(_('Check Bark'), function () { runAndShow(['-test', 'bark']); })
		]));
		body.appendChild(E('p', { 'class': 'note' },
			_('MAX SMS login is available in the built-in interface only (enable ui_enabled there).')));

		body.appendChild(E('h3', _('Service control')));
		const controlRow = E('div', { 'class': 'cbi-section' });
		for (const svc of ['vk', 'max', 'tg', 'bark']) {
			controlRow.appendChild(button(svc.toUpperCase() + ': ' + _('on'), function () {
				ctlAndShow(['control', '-service', svc, '-action', 'on']);
			}));
			controlRow.appendChild(button(svc.toUpperCase() + ': ' + _('off'), function () {
				ctlAndShow(['control', '-service', svc, '-action', 'off']);
			}));
		}
		controlRow.appendChild(button(_('Send test to Telegram'), function () {
			ctlAndShow(['send', '-service', 'tg', '-text', 'проверка']);
		}));
		controlRow.appendChild(button(_('Send test to Bark'), function () {
			ctlAndShow(['send', '-service', 'bark', '-text', 'проверка']);
		}));
		body.appendChild(controlRow);

		for (const sec of sections) {
			body.appendChild(E('h3', sec.title));

			for (const [name, label, type] of sec.fields) {
				const input = fieldInput(name, type, cfg);
				input.dataset.type = type || 'text';
				inputs[name] = input;

				const row = E('div', { 'class': 'cbi-row' }, [
					E('label', { 'for': 'field-' + name }, label),
					input
				]);
				input.id = 'field-' + name;
				body.appendChild(row);
			}
		}

		body.appendChild(E('h3', _('Debug')));
		body.appendChild(E('div', { 'class': 'cbi-section' }, [
			button(_('Debug status'), function () {
				ctlAndShow(['debug', '-action', 'status']);
			}),
			button(_('Debug on'), function () {
				ctlAndShow(['debug', '-action', 'on']);
			}),
			button(_('Debug off'), function () {
				ctlAndShow(['debug', '-action', 'off']);
			}),
			button(_('Debug log'), function () {
				ctlAndShow(['debug', '-action', 'log']);
			})
		]));

		body.appendChild(E('h3', _('JSON editor')));
		const raw = E('textarea', { 'class': 'mf-raw', rows: 8 });
		raw.value = JSON.stringify(cfg, null, 2);
		body.appendChild(raw);
		body.appendChild(E('p', { 'class': 'note' }, _('Secrets are visible here. Do not share this page.')));
		body.appendChild(E('div', { 'class': 'cbi-section' }, [
			button(_('Save raw JSON'), function () {
				let next;
				try {
					next = JSON.parse(raw.value);
				}
				catch (e) {
					show(_('Invalid JSON: ') + e.message);
					return;
				}
				fs.write(CONFIG, JSON.stringify(next, null, 2), 0o600).then(function () {
					return ctl(['status']);
				}).then(function () {
					show(_('Saved'));
				}).catch(function (e) {
					show(_('Error: ') + e.message);
				});
			})
		]));

		const save = button(_('Save and restart'), function () {
			const next = collect(cfg, inputs);
			fs.write(CONFIG, JSON.stringify(next, null, 2), 0o600)
				.then(function () {
					return fs.exec('/etc/init.d/Msg-forwarder-Go', ['restart'])
						.catch(function () {
							return fs.exec('/etc/Msg-forwarder-Go/start.sh', []);
						});
				})
				.then(function () {
					show(_('Configuration saved'));
					ui.addNotification(null, E('p', _('Configuration saved')));
				})
				.catch(function (e) {
					show(_('Failed to save: ') + e.message);
				});
		}, 'btn-action');

		body.appendChild(E('div', { 'class': 'cbi-section' }, save));
		body.appendChild(E('p', {}, [
			_('Token for the built-in interface:'),
			' ',
			E('code', {}, BIN + ' -config ' + CONFIG + ' -ui-token')
		]));
		body.appendChild(result);

		const style = E('style', {},
			'.mf-result { background:#f6f6f6; border:1px solid #ddd; padding:8px; min-height:2em; white-space:pre-wrap; }' +
			'.mf-raw { width:100%; font-family:monospace; }' +
			'.mf-status { background:#f6f6f6; border:1px solid #ddd; padding:8px; white-space:pre-wrap; }' +
			'.note { color:#666; font-size:12px; }' +
			'.btn-action { margin-top:8px; }');

		return E('div', {}, [
			style,
			E('h2', _('Msg-forwarder-Go')),
			body
		]);
	}
});
