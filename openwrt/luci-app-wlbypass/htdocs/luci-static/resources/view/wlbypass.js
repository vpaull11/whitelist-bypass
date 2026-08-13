'use strict';
'require view';
'require form';
'require uci';
'require fs';
'require rpc';

var callServiceList = rpc.declare({
	object: 'service',
	method: 'list',
	params: ['name'],
	expect: { '': {} }
});

var callInitAction = rpc.declare({
	object: 'luci',
	method: 'setInitAction',
	params: ['name', 'action'],
	expect: { result: false }
});

function renderStatus(isRunning) {
	var icon = isRunning ? '🟢' : '🔴';
	var text = isRunning ? _('Running') : _('Stopped');
	return E('div', { 'class': 'cbi-section' }, [
		E('div', { 'style': 'display:flex;align-items:center;gap:8px;font-size:1.1em;margin-bottom:12px' }, [
			E('span', {}, icon),
			E('strong', {}, _('Service Status') + ': '),
			E('span', {}, text)
		])
	]);
}

return view.extend({
	load: function () {
		return Promise.all([
			callServiceList('wlbypass'),
			uci.load('wlbypass')
		]);
	},

	render: function (data) {
		var svcData = data[0];
		var isRunning = !!(svcData && svcData.wlbypass &&
			svcData.wlbypass.instances && Object.keys(svcData.wlbypass.instances).length > 0);

		var m, s, o;

		m = new form.Map('wlbypass', _('WL Bypass'),
			_('Transparent VPN proxy through Telemost video call tunnel. ' +
			  'Configure the join link from your creator server and enable the service.'));

		// Status section
		m.insertBefore(renderStatus(isRunning), m.children[0]);

		// Main settings
		s = m.section(form.NamedSection, 'main', 'wlbypass', _('Settings'));
		s.anonymous = true;
		s.addremove = false;

		o = s.option(form.Flag, 'enabled', _('Enable'),
			_('Start the WL Bypass transparent proxy service'));
		o.rmempty = false;

		o = s.option(form.Value, 'link', _('Telemost Link'),
			_('Conference link from the creator server (e.g. https://telemost.yandex.ru/j/...)'));
		o.placeholder = 'https://telemost.yandex.ru/j/12345678901234';
		o.rmempty = false;
		o.validate = function (section, value) {
			if (!value || value.trim() === '')
				return _('A Telemost join link is required');
			return true;
		};

		o = s.option(form.ListValue, 'resources', _('Resources'),
			_('Memory and buffer allocation mode'));
		o.value('moderate', _('Moderate (64 MB) — recommended for routers'));
		o.value('default', _('Default (128 MB)'));
		o.value('unlimited', _('Unlimited (256 MB)'));
		o.default = 'moderate';

		// Advanced settings
		s = m.section(form.NamedSection, 'main', 'wlbypass', _('Advanced'));
		s.anonymous = true;
		s.addremove = false;

		o = s.option(form.Value, 'lan_iface', _('LAN Interface'),
			_('Network interface for LAN traffic redirection'));
		o.placeholder = 'br-lan';
		o.default = 'br-lan';

		o = s.option(form.Value, 'dns', _('DNS Servers'),
			_('Comma-separated DNS servers for tunneled clients'));
		o.placeholder = '1.1.1.1,8.8.8.8';
		o.default = '1.1.1.1,8.8.8.8';

		o = s.option(form.Flag, 'debug', _('Debug Logging'),
			_('Enable verbose debug output (increases log volume)'));
		o.default = '0';

		return m.render();
	}
});
