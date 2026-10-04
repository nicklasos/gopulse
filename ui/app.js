(function () {
  'use strict';

  var REFRESH_MS = 10000;
  var main = document.getElementById('cards');
  var active = null;

  function el(tag, cls, text) {
    var node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text != null) node.textContent = text;
    return node;
  }

  function clear(plot) {
    plot.querySelectorAll('.cross, .tip, .hover-dot').forEach(function (n) { n.remove(); });
    plot.querySelectorAll('.bars i.on').forEach(function (n) { n.classList.remove('on'); });
  }

  function show(plot, clientX) {
    var data = plot._hover;
    if (!data) {
      try { data = plot._hover = JSON.parse(plot.dataset.hover); } catch (e) { return; }
    }
    var n = data.labels.length;
    if (!n) return;
    var rect = plot.getBoundingClientRect();
    var frac = Math.min(Math.max((clientX - rect.left) / rect.width, 0), 1);
    var i, x;
    if (data.bars) {
      i = Math.min(Math.floor(frac * n), n - 1);
      x = (i + 0.5) / n * rect.width;
    } else {
      i = n > 1 ? Math.round(frac * (n - 1)) : 0;
      x = n > 1 ? i / (n - 1) * rect.width : rect.width / 2;
    }

    clear(plot);
    if (data.bars) {
      var bar = plot.querySelectorAll('.bars i')[i];
      if (bar) bar.classList.add('on');
    } else {
      var cross = el('div', 'cross');
      cross.style.left = x + 'px';
      plot.appendChild(cross);
    }

    var tip = el('div', 'tip');
    tip.appendChild(el('div', 't', data.labels[i]));
    data.series.forEach(function (s, si) {
      var row = el('div', 'row');
      row.appendChild(el('i', 'sw s' + s.slot));
      row.appendChild(el('span', null, s.name));
      row.appendChild(el('b', null, s.values[i]));
      tip.appendChild(row);
      if (!(data.bars && si === 0) && s.y[i] >= 0) {
        var dot = el('div', 'hover-dot s' + s.slot);
        dot.style.left = x + 'px';
        dot.style.top = (100 - s.y[i]) + '%';
        plot.appendChild(dot);
      }
    });
    plot.appendChild(tip);
    var left = x + 12;
    if (left + tip.offsetWidth > rect.width) left = x - 12 - tip.offsetWidth;
    tip.style.left = Math.max(left, 0) + 'px';
  }

  document.addEventListener('mousemove', function (e) {
    var plot = e.target.closest ? e.target.closest('.plot[data-hover]') : null;
    if (active && active !== plot) clear(active);
    active = plot;
    if (plot) show(plot, e.clientX);
  });
  document.addEventListener('mouseleave', function () {
    if (active) clear(active);
    active = null;
  });

  function refresh() {
    if (document.hidden || active || !main) return;
    var url = new URL(window.location.href);
    url.searchParams.set('_fragment', '1');
    fetch(url, { credentials: 'same-origin', cache: 'no-store' })
      .then(function (res) { return res.ok ? res.text() : Promise.reject(res.status); })
      .then(function (html) { if (!active) main.innerHTML = html; })
      .catch(function () {});
  }
  setInterval(refresh, REFRESH_MS);
})();
