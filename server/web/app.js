'use strict';

// The whole web: the NAS card, the list of services and the dialogs. It builds
// the page with DOM calls, never with innerHTML, so nothing that comes from a
// service or a person can run as code here.

const state = { plugins: [], nas: null, list: [], editing: null };

const $ = (id) => document.getElementById(id);

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'checked' || k === 'value') el[k] = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null || child === false) continue;
    el.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return el;
}

async function api(method, path, body) {
  const options = { method, headers: { 'X-Suitcase': '1' } };
  if (body !== undefined) {
    options.headers['Content-Type'] = 'application/json';
    options.body = JSON.stringify(body);
  }
  const response = await fetch('/api' + path, options);
  if (response.status === 204) return null;
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || 'Algo ha fallado (' + response.status + ')');
  return data;
}

// ---- Words for times and sizes ----

function ago(seconds) {
  if (!seconds) return 'nunca';
  const diff = Math.round(Date.now() / 1000 - seconds);
  if (diff < 60) return 'hace un momento';
  if (diff < 3600) return 'hace ' + Math.round(diff / 60) + ' min';
  if (diff < 86400) return 'hace ' + Math.round(diff / 3600) + ' h';
  return 'hace ' + Math.round(diff / 86400) + ' días';
}

function until(seconds) {
  const diff = Math.round(seconds - Date.now() / 1000);
  if (diff <= 60) return 'en breve';
  if (diff < 3600) return 'en ' + Math.round(diff / 60) + ' min';
  if (diff < 86400) return 'en ' + Math.round(diff / 3600) + ' h';
  return 'en ' + Math.round(diff / 86400) + ' días';
}

function size(bytes) {
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (bytes >= 1024 && i < units.length - 1) { bytes /= 1024; i++; }
  return (i ? bytes.toFixed(1) : bytes) + ' ' + units[i];
}

function every(hours) {
  if (hours === 1) return 'cada hora';
  if (hours < 24) return 'cada ' + hours + ' h';
  if (hours === 24) return 'cada día';
  if (hours === 168) return 'cada semana';
  return 'cada ' + Math.round(hours / 24) + ' días';
}

function volumeName(id) {
  const volume = (state.nas && state.nas.volumes || []).find((v) => v.id === id);
  return volume ? volume.name : id;
}

// ---- NAS ----

function renderNAS() {
  const nas = state.nas;
  const box = $('nas');
  box.replaceChildren();
  if (!nas || !nas.paired) {
    box.append(h('div', { class: 'card-body' },
      h('div', { class: 'card-head' },
        h('div', {}, h('h3', {}, 'Aún no está emparejado'),
          h('p', { class: 'card-sub' }, 'Este programa escribe en tu NAS igual que la app del móvil: con un código que creas en la web del NAS.'))),
    ), h('div', { class: 'card-actions' }, h('button', { class: 'primary', onclick: openPair }, 'Emparejar con el NAS')));
    return;
  }
  let tag = h('span', { class: 'tag ok' }, 'Conectado');
  let note = null;
  if (!nas.reachable) {
    tag = h('span', { class: 'tag bad' }, 'Sin conexión');
    note = h('p', { class: 'note bad' }, nas.error || 'No responde.');
  } else if (!nas.active) {
    tag = h('span', { class: 'tag bad' }, 'En espera');
    note = h('p', { class: 'note' }, 'Esta máquina del NAS está en espera: las copias esperarán a que vuelva a escribir.');
  }
  const volumes = (nas.volumes || []).map((v) => h('div', {},
    h('dt', {}, v.name),
    h('dd', {}, size(v.used) + (v.quota ? ' de ' + size(v.quota) : ' usados')),
    v.quota ? h('div', { class: 'meter' }, (() => { const s = h('span'); s.style.width = Math.min(100, 100 * v.used / v.quota) + '%'; return s; })()) : null));
  box.append(h('div', { class: 'card-body' },
    h('div', { class: 'card-head' },
      h('div', {}, h('h3', {}, nas.name || 'NAS'),
        h('p', { class: 'card-sub' }, (nas.user ? 'Como ' + nas.user + ' · ' : '') + (nas.last_address || (nas.addresses || [])[0] || ''))),
      tag),
    note,
    volumes.length ? h('dl', { class: 'facts' }, volumes) : null,
  ), h('div', { class: 'card-actions' },
    h('button', { class: 'small-button', onclick: openPair }, 'Cambiar de código'),
    h('span', { class: 'spacer' }),
    h('button', { class: 'small-button danger', onclick: () => confirmAction('¿Olvidar este NAS?', 'Las copias se pararán hasta que emparejes otra vez. Lo que ya está en el NAS no se toca. Borra también el código en la web del NAS.', 'Olvidar', async () => { await api('DELETE', '/nas'); await refresh(); }) }, 'Olvidar')));
}

function openPair() {
  $('pair-code').value = '';
  $('pair-error').hidden = true;
  $('pair').showModal();
}

$('pair').querySelector('form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const button = $('pair-ok');
  button.disabled = true;
  button.textContent = 'Comprobando…';
  try {
    state.nas = await api('POST', '/nas', { code: $('pair-code').value });
    $('pair').close();
    await refresh();
  } catch (error) {
    $('pair-error').textContent = error.message;
    $('pair-error').hidden = false;
  } finally {
    button.disabled = false;
    button.textContent = 'Emparejar';
  }
});

// ---- Services ----

function statusOf(item) {
  if (item.unknown_kind) return h('span', { class: 'tag bad' }, 'Desconocido');
  if (item.running) return h('span', { class: 'tag accent' }, 'Copiando…');
  if (!item.enabled) return h('span', { class: 'tag' }, 'En pausa');
  if (item.last_error) return h('span', { class: 'tag bad' }, 'Con problemas');
  if (item.last_ok) return h('span', { class: 'tag ok' }, 'Al día');
  return h('span', { class: 'tag' }, 'Pendiente');
}

function renderList() {
  const list = $('list');
  list.replaceChildren();
  if (!state.list.length) {
    list.append(h('div', { class: 'card' }, h('div', { class: 'empty' },
      h('strong', {}, 'Todavía no hay servicios'),
      'Añade tu blog, tu archivo de X, tus chats de Telegram o tu cuenta de GitHub, y se irán copiando solos a tu NAS.')));
    return;
  }
  for (const item of state.list) {
    const files = item.last_files || {};
    const last = item.last_end
      ? ago(item.last_end) + ' · ' + item.last_items + ' nuevos, ' + files.created + ' ficheros nuevos' + (files.updated ? ', ' + files.updated + ' actualizados' : '')
      : 'nunca';
    list.append(h('article', { class: 'card' },
      h('div', { class: 'card-body' },
        h('div', { class: 'card-head' },
          h('div', {}, h('h3', {}, item.name), h('p', { class: 'card-sub' }, item.service + ' → ' + volumeName(item.volume) + ' · ' + item.folder)),
          statusOf(item)),
        h('dl', { class: 'facts' },
          h('div', {}, h('dt', {}, 'Última copia'), h('dd', {}, item.running ? 'ahora mismo' : last)),
          h('div', {}, h('dt', {}, 'Próxima'), h('dd', {}, item.enabled ? until(item.next_run) + ' (' + every(item.every_hours) + ')' : 'en pausa')),
          h('div', {}, h('dt', {}, 'Guardados en total'), h('dd', {}, item.total_items + ' elementos, ' + item.total_files + ' ficheros'))),
        item.last_error ? h('p', { class: 'note error' }, item.last_error) : null),
      h('div', { class: 'card-actions' },
        h('button', { class: 'small-button primary', disabled: item.running || !state.nas || !state.nas.paired, onclick: () => runNow(item) }, item.running ? 'Copiando…' : 'Ejecutar ahora'),
        h('button', { class: 'small-button', onclick: () => showLog(item) }, 'Registro'),
        h('button', { class: 'small-button', onclick: () => openEdit(item), disabled: item.unknown_kind }, 'Editar'),
        h('span', { class: 'spacer' }),
        h('button', { class: 'small-button danger', onclick: () => confirmAction('¿Quitar «' + item.name + '»?', 'Dejará de copiarse. Lo que ya está en el NAS se queda donde está.', 'Quitar', async () => { await api('DELETE', '/integrations/' + item.id); await refresh(); }) }, 'Quitar'))));
  }
}

async function runNow(item) {
  try {
    await api('POST', '/integrations/' + item.id + '/run');
  } catch (error) {
    alert(error.message);
  }
  await refresh();
}

async function showLog(item) {
  $('log-title').textContent = 'Registro de ' + item.name;
  const lines = await api('GET', '/integrations/' + item.id + '/log');
  const ul = $('log-lines');
  ul.replaceChildren(...(lines.length ? lines.slice().reverse().map((line) => h('li', {},
    h('time', {}, new Date(line.time * 1000).toLocaleString('es-ES', { dateStyle: 'short', timeStyle: 'short' })),
    h('span', {}, line.text))) : [h('li', {}, h('span', {}, 'Aún no se ha ejecutado.'))]));
  $('log').showModal();
}

// ---- Add and edit ----

function openEdit(item) {
  state.editing = item || null;
  $('edit-title').textContent = item ? 'Editar «' + item.name + '»' : 'Añadir un servicio';
  if (item) {
    renderForm(state.plugins.find((p) => p.kind === item.kind), item);
  } else {
    renderChooser();
  }
  $('edit').showModal();
}

function renderChooser() {
  $('edit-ok').hidden = true;
  $('edit-body').replaceChildren(
    h('p', { class: 'hint' }, '¿De qué servicio quieres guardar una copia?'),
    h('div', { class: 'services' }, state.plugins.map((p) => h('button', { type: 'button', class: 'service', onclick: () => renderForm(p, null) },
      h('strong', {}, p.title), h('small', {}, p.description)))),
    h('p', { class: 'field-hint' }, 'LinkedIn, Google (Gmail, Drive, Fotos) y YouTube se pueden guardar ya con su exportación oficial y «Carpeta local»: mira el README.'));
}

function fieldInput(field, item) {
  const id = 'f-' + field.name;
  const value = item ? (item.config[field.name] ?? '') : (field.default || '');
  if (field.type === 'checkbox') {
    return h('label', { class: 'check' },
      h('input', { type: 'checkbox', id, name: field.name, checked: (item ? (item.config[field.name] ?? field.default) : field.default) === 'true' }),
      h('span', {}, field.label, field.help ? h('p', { class: 'field-hint' }, field.help) : null));
  }
  const saved = item && field.secret && item.secrets_set.includes(field.name);
  const attrs = { id, name: field.name, placeholder: saved ? 'Guardado. Déjalo vacío para no cambiarlo' : (field.placeholder || ''), required: field.required && !saved, autocomplete: 'off', spellcheck: 'false' };
  const input = field.type === 'textarea'
    ? h('textarea', { ...attrs, rows: 4 })
    : h('input', { ...attrs, type: field.type === 'password' ? 'password' : field.type === 'url' ? 'url' : 'text' });
  if (!field.secret) input.value = value;
  return h('label', { class: 'field' },
    h('span', { class: 'field-label' }, field.label + (field.required ? '' : ' (opcional)')),
    input,
    field.help ? h('p', { class: 'field-hint' }, field.help) : null);
}

function renderForm(plugin, item) {
  state.form = plugin;
  $('edit-ok').hidden = false;
  const volumes = (state.nas && state.nas.volumes) || [];
  const defaultVolume = item ? item.volume : (state.nas && state.nas.default_volume) || (volumes[0] && volumes[0].id) || '';
  const hours = item ? item.every_hours : plugin.default_every_hours;
  const options = [1, 3, 6, 12, 24, 72, 168];
  if (!options.includes(hours)) options.push(hours);
  $('edit-body').replaceChildren(
    h('p', { class: 'help' }, plugin.help),
    h('label', { class: 'field' }, h('span', { class: 'field-label' }, 'Nombre'),
      h('input', { type: 'text', id: 'f-name', value: item ? item.name : plugin.title, maxlength: 80 })),
    ...plugin.fields.map((field) => fieldInput(field, item)),
    h('div', { class: 'row2' },
      h('label', { class: 'field' }, h('span', { class: 'field-label' }, 'Volumen del NAS'),
        volumes.length
          ? h('select', { id: 'f-volume' }, volumes.map((v) => h('option', { value: v.id, selected: v.id === defaultVolume }, v.name)))
          : h('input', { type: 'text', id: 'f-volume', value: defaultVolume, placeholder: 'principal' })),
      h('label', { class: 'field' }, h('span', { class: 'field-label' }, 'Carpeta'),
        h('input', { type: 'text', id: 'f-folder', value: item ? item.folder : plugin.default_folder, required: true }))),
    h('label', { class: 'field' }, h('span', { class: 'field-label' }, 'Cada cuánto'),
      h('select', { id: 'f-every' }, options.sort((a, b) => a - b).map((n) => h('option', { value: n, selected: n === hours }, every(n))))),
    h('label', { class: 'check' }, h('input', { type: 'checkbox', id: 'f-enabled', checked: item ? item.enabled : true }), h('span', {}, 'Activo')),
    h('p', { id: 'edit-error', class: 'form-error', hidden: true }));
  const volumeSelect = $('f-volume');
  if (volumeSelect.tagName === 'SELECT') volumeSelect.value = defaultVolume;
}

$('edit-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const plugin = state.form;
  if (!plugin) return;
  const config = {};
  for (const field of plugin.fields) {
    const el = $('f-' + field.name);
    config[field.name] = field.type === 'checkbox' ? String(el.checked) : el.value;
  }
  const input = {
    kind: plugin.kind,
    name: $('f-name').value,
    volume: $('f-volume').value,
    folder: $('f-folder').value,
    every_hours: Number($('f-every').value),
    enabled: $('f-enabled').checked,
    config,
  };
  const button = $('edit-ok');
  button.disabled = true;
  button.textContent = 'Comprobando…';
  try {
    if (state.editing) await api('PUT', '/integrations/' + state.editing.id, input);
    else await api('POST', '/integrations', input);
    $('edit').close();
    await refresh();
  } catch (error) {
    $('edit-error').textContent = error.message;
    $('edit-error').hidden = false;
  } finally {
    button.disabled = false;
    button.textContent = 'Guardar';
  }
});

// ---- Confirmations and closing ----

function confirmAction(title, text, label, action) {
  $('confirm-title').textContent = title;
  $('confirm-text').textContent = text;
  const ok = $('confirm-ok');
  ok.textContent = label;
  ok.onclick = async () => {
    ok.disabled = true;
    try { await action(); $('confirm').close(); } catch (error) { alert(error.message); } finally { ok.disabled = false; }
  };
  $('confirm').showModal();
}

for (const button of document.querySelectorAll('[data-close]')) {
  button.addEventListener('click', () => button.closest('dialog').close());
}

$('add').addEventListener('click', () => openEdit(null));

// ---- Refresh ----

let timer = null;

async function refresh() {
  clearTimeout(timer);
  try {
    const [nas, list] = await Promise.all([api('GET', '/nas'), api('GET', '/integrations')]);
    state.nas = nas;
    state.list = list;
    renderNAS();
    renderList();
  } catch (error) {
    $('nas').replaceChildren(h('div', { class: 'card-body' }, h('p', { class: 'note error' }, 'No se puede hablar con el programa: ' + error.message)));
  }
  // Faster while something is copying, so the counts move.
  timer = setTimeout(refresh, state.list.some((i) => i.running) ? 2000 : 30000);
}

(async () => {
  state.plugins = await api('GET', '/plugins');
  await refresh();
})();
