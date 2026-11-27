'use strict';
'require view';
'require fs';
'require ui';

const CONFIG = '/etc/Msg-forwarder-Go/config.json';

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

		if (type === 'secret' && cfg[name]) {
			input.placeholder = _('configured');
		}
		else if (cfg[name] !== undefined && cfg[name] !== null) {
			input.value = String(cfg[name]);
		}
	}

	if (type === 'number' && cfg[name] !== undefined)
		input.value = String(cfg[name]);

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
		const body = E('div', { 'class': 'cbi-section' });

		for (const sec of sections) {
			body.appendChild(E('h3', sec.title));

			for (const [name, label, type] of sec.fields) {
				const input = fieldInput(name, type, cfg);
				if (input.dataset)
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

		body.appendChild(E('p', {}, [
			_('Token for the built-in interface:'),
			' ',
			E('code', {}, '/usr/bin/Msg-forwarder-Go -config /etc/Msg-forwarder-Go/config.json -ui-token')
		]));

		const save = E('button', { 'class': 'btn', 'type': 'button' }, _('Save and restart'));
		save.addEventListener('click', function () {
			const next = collect(cfg, inputs);
			fs.write(CONFIG, JSON.stringify(next, null, 2), 0o600)
				.then(function () {
					return fs.exec('/etc/init.d/Msg-forwarder-Go', ['restart'])
						.catch(function () {
							return fs.exec('/etc/Msg-forwarder-Go/start.sh', []);
						});
				})
				.then(function () {
					ui.addNotification(null, E('p', _('Configuration saved')));
				})
				.catch(function (e) {
					ui.addNotification(null, E('p', _('Failed to save: ') + e.message), 'error');
				});
		});

		body.appendChild(E('div', { 'class': 'cbi-section' }, save));

		return E('div', {}, [
			E('h2', _('Msg-forwarder-Go')),
			body
		]);
	}
});
