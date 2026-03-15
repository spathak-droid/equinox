(function() {
  if (window.__livePriceState) return;
  var historyByKey = {};
  var latestByKey = {};
  var maxPoints = 120;

  function mkKey(venue, marketId) {
    return String((venue || "").toLowerCase()) + ":" + String(marketId || "");
  }

  function publish(venue, marketId, yesProb) {
    if (typeof yesProb !== "number" || !isFinite(yesProb) || !marketId) return;
    if (yesProb < 0) yesProb = 0;
    if (yesProb > 1) yesProb = 1;
    var key = mkKey(venue, marketId);
    if (!historyByKey[key]) historyByKey[key] = [];
    historyByKey[key].push({ t: Date.now(), p: yesProb });
    if (historyByKey[key].length > maxPoints) historyByKey[key].shift();
    latestByKey[key] = yesProb;
    window.dispatchEvent(new CustomEvent("equinox-price-tick", { detail: { key: key, p: yesProb } }));
  }

  window.__livePriceState = {
    key: mkKey,
    publish: publish,
    latest: function(venue, marketId) { return latestByKey[mkKey(venue, marketId)]; },
    history: function(venue, marketId) { return (historyByKey[mkKey(venue, marketId)] || []).slice(); }
  };

  // When a live price arrives, update any route-form that references this market
  // and recalculate the routing decision with fresh prices.
  window.addEventListener("equinox-price-tick", function(evt) {
    var d = evt.detail;
    if (!d || !d.p) return;
    var parts = d.key.split(":");
    var venue = parts[0] || "";
    var marketId = parts.slice(1).join(":") || "";
    if (!marketId) return;

    var forms = document.querySelectorAll(".route-form");
    for (var i = 0; i < forms.length; i++) {
      var f = forms[i];
      if (f.dataset.marketIdA === marketId && (f.dataset.venueA || "").toLowerCase() === venue) {
        f.dataset.yesA = String(d.p);
        f.dataset.noA = String(1 - d.p);
        var btn = f.querySelector(".route-calc-btn");
        if (btn && window.recalcRoute) window.recalcRoute(btn);
      }
      if (f.dataset.marketIdB === marketId && (f.dataset.venueB || "").toLowerCase() === venue) {
        f.dataset.yesB = String(d.p);
        f.dataset.noB = String(1 - d.p);
        var btn2 = f.querySelector(".route-calc-btn");
        if (btn2 && window.recalcRoute) window.recalcRoute(btn2);
      }
    }
  });
})();

(function() {
  window.toggleExplain = function(btn, id) {
    var el = document.getElementById(id);
    if (!el) return;
    var open = el.classList.toggle("is-open");
    btn.classList.toggle("is-open", open);
  };

  window.loadMoreNews = function(btn) {
    var container = btn.closest(".pair-news");
    if (!container) return;
    var query = container.getAttribute("data-news-query");
    if (!query) return;
    btn.disabled = true;
    btn.classList.add("is-loading");
    btn.querySelector(".material-icons-round").textContent = "sync";
    btn.childNodes[btn.childNodes.length - 1].textContent = " Loading...";

    fetch("/news?q=" + encodeURIComponent(query))
      .then(function(r) { return r.json(); })
      .then(function(data) {
        var list = container.querySelector(".news-list");
        if (!list) return;
        list.innerHTML = "";
        var articles = data.articles || [];
        if (articles.length === 0) {
          list.innerHTML = '<li class="news-item" style="color:var(--text-muted);font-size:0.73rem;">No articles found</li>';
        }
        for (var i = 0; i < articles.length; i++) {
          var a = articles[i];
          var li = document.createElement("li");
          li.className = "news-item";
          li.innerHTML = '<div class="news-item-header">' +
            '<span class="news-item-title"><a href="' + escAttr(a.url) + '" target="_blank" rel="noopener">' + escTxt(a.title) + '</a></span>' +
            '<span class="news-item-meta">' + (a.source ? '<span class="news-item-source">' + escTxt(a.source) + '</span>' : '') + (a.age ? ' \u00b7 <span class="news-item-age">' + escTxt(a.age) + '</span>' : '') + '</span>' +
            '</div>';
          list.appendChild(li);
        }
        btn.remove();
      })
      .catch(function() {
        btn.disabled = false;
        btn.classList.remove("is-loading");
        btn.querySelector(".material-icons-round").textContent = "add";
        btn.childNodes[btn.childNodes.length - 1].textContent = " See more";
      });
  };

  function escTxt(s) { var d = document.createElement("div"); d.textContent = s || ""; return d.innerHTML; }
  function escAttr(s) { return (s || "").replace(/&/g,"&amp;").replace(/"/g,"&quot;").replace(/</g,"&lt;").replace(/>/g,"&gt;"); }

  var modal = document.getElementById("marketDetailModal");
  if (!modal) return;

  var fields = {};
  ["mdTitle","mdVenue","mdVenueBadge","mdMarketId","mdStatus","mdDescription","mdTags",
   "mdCategory","mdResolutionDate","mdResolutionCriteria","mdYes","mdNo",
   "mdLiquidity","mdSpread","mdVolume",
   "mdOpenInterest","mdRawPayload","mdLinks",
   "mdLiveNow","mdLiveDelta","mdLiveSection"].forEach(function(id) {
    fields[id] = document.getElementById(id);
  });

  function safe(v) { return v ? String(v) : "--"; }
  var activeLive = { venue: "", marketId: "" };

  function renderLivePrice(venue, marketId) {
    var st = window.__livePriceState;
    if (!st) { fields.mdLiveSection.style.display = "none"; return; }
    var latest = st.latest(venue, marketId);
    if (latest == null) { fields.mdLiveSection.style.display = "none"; return; }
    fields.mdLiveSection.style.display = "";
    fields.mdLiveNow.textContent = (latest * 100).toFixed(1) + "%";

    var hist = st.history(venue, marketId);
    if (hist.length >= 2) {
      var first = hist[0].p;
      var diff = latest - first;
      var cls = "flat";
      if (diff > 0.0001) cls = "up";
      else if (diff < -0.0001) cls = "down";
      var sign = diff > 0 ? "+" : "";
      fields.mdLiveDelta.textContent = sign + (diff * 100).toFixed(2) + "%";
      fields.mdLiveDelta.className = "live-delta " + cls;
    } else {
      fields.mdLiveDelta.textContent = "live";
      fields.mdLiveDelta.className = "live-delta flat";
    }
  }

  function fmtPctModal(v) {
    var n = parseFloat(v);
    if (!isFinite(n)) return "--";
    return (n * 100).toFixed(1) + "%";
  }
  function fmtUsdModal(v) {
    var n = parseFloat(v);
    if (!isFinite(n) || n === 0) return "--";
    if (n >= 1e6) return "$" + (n / 1e6).toFixed(1) + "M";
    if (n >= 1e3) return "$" + (n / 1e3).toFixed(1) + "K";
    return "$" + n.toFixed(0);
  }
  function venueDisplayName(v) {
    if (v === "polymarket") return "Polymarket";
    if (v === "kalshi") return "Kalshi";
    return v || "--";
  }

  window.showMarketModal = function(card) {
    var d = card.dataset;
    var venueName = venueDisplayName(d.venue);
    fields.mdTitle.textContent = safe(d.title);
    fields.mdVenue.textContent = venueName;
    fields.mdMarketId.textContent = safe(d.marketId);
    fields.mdStatus.textContent = safe(d.status);
    fields.mdDescription.textContent = safe(d.description);
    fields.mdTags.textContent = safe(d.tags);
    fields.mdCategory.textContent = safe(d.category);
    fields.mdResolutionDate.textContent = safe(d.resolutionDate);
    fields.mdResolutionCriteria.textContent = safe(d.resolutionCriteria);
    fields.mdVolume.textContent = fmtUsdModal(d.volume24h);
    fields.mdOpenInterest.textContent = fmtUsdModal(d.openInterest);
    fields.mdYes.textContent = fmtPctModal(d.yes);
    fields.mdNo.textContent = fmtPctModal(d.no);
    fields.mdLiquidity.textContent = fmtUsdModal(d.liquidity);
    fields.mdSpread.textContent = fmtPctModal(d.spread);

    // Venue badge
    var badge = fields.mdVenueBadge;
    badge.textContent = venueName;
    badge.className = "modal-venue-badge vb-" + (d.venue || "").toLowerCase();

    activeLive.venue = String(d.venue || "").toLowerCase();
    activeLive.marketId = String(d.marketId || "");
    var initYes = parseFloat(d.yes);
    if (window.__livePriceState && isFinite(initYes)) {
      window.__livePriceState.publish(activeLive.venue, activeLive.marketId, initYes);
    }
    renderLivePrice(activeLive.venue, activeLive.marketId);

    var b64 = d.payload || "";
    if (b64) {
      try {
        fields.mdRawPayload.textContent = JSON.stringify(JSON.parse(atob(b64)), null, 2);
      } catch(e) { fields.mdRawPayload.textContent = b64; }
    } else {
      fields.mdRawPayload.textContent = "No raw payload available.";
    }

    var links = fields.mdLinks;
    links.innerHTML = "";
    var venueLink = safe(d.venueLink);
    if (venueLink && venueLink !== "--") {
      links.innerHTML += '<a class="modal-link" href="' + escAttr(venueLink) + '" target="_blank" rel="noopener"><span class="material-icons-round">open_in_new</span>' + escTxt(venueName) + '</a>';
    }

    modal.classList.add("is-open");
    modal.setAttribute("aria-hidden", "false");
    document.body.style.overflow = "hidden";
  }

  document.querySelectorAll(".clickable-market").forEach(function(card) {
    card.addEventListener("click", function() { window.showMarketModal(card); });
  });

  window.closeMarketModal = function() {
    modal.classList.remove("is-open");
    modal.setAttribute("aria-hidden", "true");
    document.body.style.overflow = "";
    activeLive.venue = "";
    activeLive.marketId = "";
  };

  window.addEventListener("keydown", function(e) {
    if (e.key === "Escape") window.closeMarketModal();
  });

  document.querySelectorAll(".pair-card").forEach(function(card, i) {
    card.style.opacity = "0";
    card.style.transform = "translateY(16px)";
    card.style.transition = "opacity 400ms ease " + (i * 60) + "ms, transform 400ms ease " + (i * 60) + "ms";
  });
  var observer = new IntersectionObserver(function(entries) {
    entries.forEach(function(entry) {
      if (entry.isIntersecting) {
        entry.target.style.opacity = "1";
        entry.target.style.transform = "translateY(0)";
      }
    });
  }, { threshold: 0.05 });
  document.querySelectorAll(".pair-card").forEach(function(card) { observer.observe(card); });

  // Auto-fetch first news article for sections that have no articles yet
  document.querySelectorAll(".pair-news[data-news-query]").forEach(function(newsEl) {
    var list = newsEl.querySelector(".news-list");
    if (!list) return;
    if (list.querySelector(".news-item a")) return;
    var query = newsEl.getAttribute("data-news-query");
    if (!query) return;
    list.innerHTML = '<li class="news-item" style="color:var(--text-muted);font-size:0.73rem;">Loading news...</li>';
    fetch("/news?q=" + encodeURIComponent(query))
      .then(function(r) { return r.json(); })
      .then(function(data) {
        var articles = data.articles || [];
        if (articles.length > 0) {
          list.innerHTML = '';
          for (var i = 0; i < Math.min(articles.length, 1); i++) {
            var a = articles[i];
            var li = document.createElement("li");
            li.className = "news-item";
            li.innerHTML = '<div class="news-item-header">' +
              '<span class="news-item-title"><a href="' + escAttr(a.url) + '" target="_blank" rel="noopener">' + escTxt(a.title) + '</a></span>' +
              '<span class="news-item-meta">' + (a.source ? '<span class="news-item-source">' + escTxt(a.source) + '</span>' : '') + (a.age ? ' \u00b7 <span class="news-item-age">' + escTxt(a.age) + '</span>' : '') + '</span>' +
            '</div>';
            list.appendChild(li);
          }
        } else {
          list.innerHTML = '<li class="news-item" style="color:var(--text-muted);font-size:0.73rem;">No articles found</li>';
        }
      })
      .catch(function() { list.innerHTML = ''; });
  });

  window.addEventListener("equinox-price-tick", function() {
    if (!activeLive.venue || !activeLive.marketId) return;
    if (!modal.classList.contains("is-open")) return;
    renderLivePrice(activeLive.venue, activeLive.marketId);
  });
})();

/* ── Live Polymarket prices (WSS) ───────────────────────── */
(function() {
  var priceEls = Array.prototype.slice.call(document.querySelectorAll(".mkt-price[data-token-id]"));
  if (!priceEls.length) return;

  var byToken = {};
  priceEls.forEach(function(el) {
    var venue = (el.dataset.venue || "").toLowerCase();
    var token = (el.dataset.tokenId || "").trim();
    if (venue !== "polymarket" || !token) return;
    if (!byToken[token]) byToken[token] = [];
    byToken[token].push(el);
  });

  var tokenIDs = Object.keys(byToken);
  if (!tokenIDs.length) return;

  function renderProbability(token, p) {
    if (typeof p !== "number" || !isFinite(p)) return;
    if (p < 0) p = 0;
    if (p > 1) p = 1;
    var txt = (p * 100).toFixed(1) + "%";
    (byToken[token] || []).forEach(function(el) {
      el.textContent = txt;
      if (window.__livePriceState) {
        window.__livePriceState.publish("polymarket", el.dataset.marketId, p);
      }
    });
  }

  function extractProb(msg) {
    function toNum(v) {
      if (typeof v === "number" && isFinite(v)) return v;
      if (typeof v === "string" && v.trim() !== "") {
        var n = parseFloat(v);
        if (isFinite(n)) return n;
      }
      return null;
    }
    var p = null;
    var price = toNum(msg.price);
    var lastTrade = toNum(msg.last_trade_price);
    var bestBid = toNum(msg.best_bid);
    var bestAsk = toNum(msg.best_ask);
    if (price != null) p = price;
    else if (lastTrade != null) p = lastTrade;
    else if (bestBid != null && bestAsk != null) p = (bestBid + bestAsk) / 2;
    else if (bestBid != null) p = bestBid;
    else if (bestAsk != null) p = bestAsk;
    if (p == null) return null;
    // Some feeds send cents-style prices. Normalize if needed.
    if (p > 1.000001) p = p / 100.0;
    return p;
  }

  var ws = new WebSocket("wss://ws-subscriptions-clob.polymarket.com/ws/market");
  ws.onopen = function() {
    // Docs have used both asset_ids and assets_ids in examples.
    var payload = { type: "market", asset_ids: tokenIDs, assets_ids: tokenIDs };
    ws.send(JSON.stringify(payload));
  };
  ws.onmessage = function(evt) {
    var data;
    try { data = JSON.parse(evt.data); } catch (_) { return; }
    var msgs = Array.isArray(data) ? data : [data];
    msgs.forEach(function(msg) {
      if (!msg || typeof msg !== "object") return;
      var token = String(msg.asset_id || msg.assetId || "").trim();
      if (!token || !byToken[token]) return;
      var prob = extractProb(msg);
      if (prob == null) return;
      renderProbability(token, prob);
    });
  };
  ws.onerror = function() {};
})();

/* ── Live Kalshi prices (WSS ticker) ─────────────────────── */
(function() {
  var priceEls = Array.prototype.slice.call(document.querySelectorAll(".mkt-price[data-market-id]"));
  if (!priceEls.length) return;

  var byTicker = {};
  priceEls.forEach(function(el) {
    var venue = (el.dataset.venue || "").toLowerCase();
    var ticker = (el.dataset.marketId || "").trim();
    if (venue !== "kalshi" || !ticker) return;
    if (!byTicker[ticker]) byTicker[ticker] = [];
    byTicker[ticker].push(el);
  });

  var tickers = Object.keys(byTicker);
  if (!tickers.length) return;

  function renderTicker(ticker, yesProb) {
    if (typeof yesProb !== "number" || !isFinite(yesProb)) return;
    if (yesProb < 0) yesProb = 0;
    if (yesProb > 1) yesProb = 1;
    var txt = (yesProb * 100).toFixed(1) + "%";
    (byTicker[ticker] || []).forEach(function(el) {
      el.textContent = txt;
      if (window.__livePriceState) {
        window.__livePriceState.publish("kalshi", ticker, yesProb);
      }
    });
  }

  var ws = new WebSocket("wss://api.elections.kalshi.com/trade-api/ws/v2");
  ws.onopen = function() {
    ws.send(JSON.stringify({
      id: 1,
      cmd: "subscribe",
      params: {
        channels: ["ticker"],
        market_tickers: tickers
      }
    }));
  };
  ws.onmessage = function(evt) {
    var data;
    try { data = JSON.parse(evt.data); } catch (_) { return; }
    if (!data || data.type !== "ticker" || !data.msg) return;

    var ticker = String(data.msg.market_ticker || "").trim();
    if (!ticker || !byTicker[ticker]) return;

    var yesBid = data.msg.yes_bid;
    var yesAsk = data.msg.yes_ask;
    if (typeof yesBid === "string" && yesBid.trim() !== "") yesBid = parseFloat(yesBid);
    if (typeof yesAsk === "string" && yesAsk.trim() !== "") yesAsk = parseFloat(yesAsk);
    var yes = null;
    if (typeof yesBid === "number" && typeof yesAsk === "number") yes = (yesBid + yesAsk) / 2 / 100.0;
    else if (typeof yesBid === "number") yes = yesBid / 100.0;
    else if (typeof yesAsk === "number") yes = yesAsk / 100.0;
    if (yes == null) return;
    renderTicker(ticker, yes);
  };
  ws.onerror = function() {};
})();

/* ── SSE search loader with incremental pair streaming ──── */
(function() {
  var content = document.querySelector(".content");

  function showStreamUI(q) {
    // Ensure header search form is visible (it's absent on the landing page)
    var headerInner = document.querySelector(".header-inner");
    if (headerInner && !headerInner.querySelector(".search-form")) {
      // Add logo if missing (landing page transition)
      if (!headerInner.querySelector(".logo")) {
        var logo = document.createElement("a");
        logo.href = "/";
        logo.className = "logo";
        logo.innerHTML =
          '<svg class="logo-icon" viewBox="0 0 36 36" xmlns="http://www.w3.org/2000/svg">' +
            '<circle cx="18" cy="18" r="15" fill="none" stroke="rgba(129,140,248,0.4)" stroke-width="2"/>' +
            '<path d="M8 22 L13 17 L17 20 L18 18" fill="none" stroke="#a78bfa" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/>' +
            '<path d="M18 18 L22 13 L26 16 L30 10" fill="none" stroke="#3b82f6" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/>' +
            '<path d="M18 18 L18 6 M15.5 8.5 L18 6 L20.5 8.5" fill="none" stroke="#34d399" stroke-width="2.5" stroke-linejoin="round" stroke-linecap="round"/>' +
          '</svg>' +
          '<span class="logo-text">EQUINOX</span>';
        headerInner.prepend(logo);
      }
      var form = document.createElement("form");
      form.className = "search-form";
      form.method = "GET";
      form.action = "/";
      form.innerHTML =
        '<input class="search-input" type="text" name="q" value="' + escHtml(q) + '" placeholder="Search markets across venues..." autofocus>' +
        '<button class="search-btn" type="submit">Search</button>';
      headerInner.appendChild(form);
      form.addEventListener("submit", function(e) {
        var input = form.querySelector("[name=q]");
        if (!input) return;
        var val = input.value.trim();
        if (!val) return;
        e.preventDefault();
        startStreamSearch(val, false);
      });
    } else if (headerInner) {
      // Update existing search input value
      var existing = headerInner.querySelector(".search-input");
      if (existing) existing.value = q;
    }

    content.innerHTML =
      '<div class="results-header" id="streamHeader">' +
        '<div class="results-title"><span class="material-icons-round" style="font-size:14px;vertical-align:middle;animation:loaderSpin 0.9s linear infinite;margin-right:4px;">sync</span> Searching for <strong>"' + escHtml(q) + '"</strong></div>' +
      '</div>' +
      '<div id="streamPairs"></div>' +
      '<div class="search-loader" id="streamLoader" style="margin-top:16px;">' +
        '<div class="loader-log" id="loaderLog"></div>' +
      '</div>';
  }

  function addLine(cls, iconCls, iconChar, msg, extra) {
    var log = document.getElementById("loaderLog");
    if (!log) return;
    var delay = log.children.length * 60;
    var line = document.createElement("div");
    line.className = "loader-line " + cls;
    line.style.animationDelay = delay + "ms";
    line.innerHTML =
      '<span class="loader-icon ' + iconCls + '">' + iconChar + '</span>' +
      '<span class="loader-msg">' + escHtml(msg) + '</span>' +
      (extra || "");
    log.appendChild(line);
  }

  function escHtml(s) {
    return String(s).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;");
  }

  function venueTag(venue) {
    if (!venue) return "";
    return '<span class="loader-venue-' + escHtml(venue) + '">' + escHtml(venue) + '</span>';
  }

  function countBadge(n) {
    if (!n) return "";
    return '<span class="loader-count">' + n + '</span>';
  }

  function fmtPct(f) { return (f * 100).toFixed(1) + "%"; }
  function fmtScore(f) { return f.toFixed(3); }
  function fmtScoreWidth(f) { return (f * 100).toFixed(1) + "%"; }
  function fmtUsd(f) {
    if (!f) return "--";
    if (f >= 1000000) return "$" + (f / 1000000).toFixed(1) + "M";
    if (f >= 1000) return "$" + (f / 1000).toFixed(1) + "K";
    return "$" + Math.round(f);
  }
  function confClass(c) { return c === "MATCH" ? "conf-match" : c === "PROBABLE_MATCH" ? "conf-probable" : "conf-no"; }
  function cardClass(c) { return c === "MATCH" ? "card-match" : c === "PROBABLE_MATCH" ? "card-probable" : ""; }
  function confIcon(c) { return c === "MATCH" ? "check_circle" : c === "PROBABLE_MATCH" ? "help" : "cancel"; }
  function venueClass(v) { return v === "polymarket" ? "venue-poly" : "venue-kalshi"; }
  function venueIcon(v) { return v === "polymarket" ? "P" : "K"; }

  function mktDataAttrs(m) {
    return ' data-venue="' + escHtml(m.venue) + '"' +
      ' data-market-id="' + escHtml(m.venue_market_id) + '"' +
      ' data-title="' + escHtml(m.title) + '"' +
      ' data-description="' + escHtml(m.description) + '"' +
      ' data-category="' + escHtml(m.category) + '"' +
      ' data-tags="' + escHtml(m.tags) + '"' +
      ' data-status="' + escHtml(m.status) + '"' +
      ' data-yes="' + (m.yes_price || 0).toFixed(6) + '"' +
      ' data-no="' + (m.no_price || 0).toFixed(6) + '"' +
      ' data-token-id="' + escHtml(m.venue_yes_token_id || "") + '"' +
      ' data-liquidity="' + (m.liquidity || 0).toFixed(2) + '"' +
      ' data-spread="' + (m.spread || 0).toFixed(6) + '"' +
      ' data-resolution-date="' + escHtml(m.resolution_date) + '"' +
      ' data-created-at="' + escHtml(m.created_at) + '"' +
      ' data-updated-at="' + escHtml(m.updated_at) + '"' +
      ' data-volume24h="' + (m.volume_24h || 0).toFixed(2) + '"' +
      ' data-open-interest="' + (m.open_interest || 0).toFixed(2) + '"' +
      ' data-resolution-criteria="' + escHtml(m.resolution_raw) + '"' +
      ' data-venue-link="' + escHtml(m.venue_link) + '"' +
      ' data-venue-search-link="' + escHtml(m.venue_search_link) + '"' +
      ' data-venue-search-link-alt="' + escHtml(m.venue_search_link_alt) + '"' +
      ' data-image-url="' + escHtml(m.image_url) + '"' +
      ' data-payload="' + escHtml(m.raw_payload_b64) + '"';
  }

  function renderMktCol(m) {
    var thumb = m.image_url ? '<img class="mkt-thumb" src="' + escHtml(m.image_url) + '" alt="" loading="lazy">' : '';
    return '<div class="market-col clickable-market"' + mktDataAttrs(m) + '>' +
      '<div class="mkt-header">' +
        thumb +
        '<div class="venue-dot vd-' + escHtml(m.venue) + '">' + venueIcon(m.venue) + '</div>' +
        '<div class="mkt-title">' + escHtml(m.title) + '</div>' +
      '</div>' +
      '<div class="mkt-stats">' +
        '<span class="mkt-price" data-venue="' + escHtml(m.venue) + '" data-market-id="' + escHtml(m.venue_market_id) + '" data-token-id="' + escHtml(m.venue_yes_token_id || "") + '"><span class="price-yes">' + fmtPct(m.yes_price || 0) + '</span> <span class="price-no">' + fmtPct(m.no_price || 0) + '</span></span>' +
        '<span class="mkt-stat">Liq <span>' + fmtUsd(m.liquidity) + '</span></span>' +
        '<span class="mkt-stat">Spread <span>' + (m.spread ? fmtPct(m.spread) : "--") + '</span></span>' +
        (m.resolution_date ? '<span class="mkt-stat">Res <span>' + escHtml(m.resolution_date) + '</span></span>' : '') +
        (m.volume_24h >= 1000 ? '<span class="mkt-stat">24h <span>' + fmtUsd(m.volume_24h) + '</span></span>' : '') +
      '</div>' +
    '</div>';
  }

  function renderNewsArticle(a) {
    return '<li class="news-item"><div class="news-item-header">' +
      '<span class="news-item-title"><a href="' + escHtml(a.url) + '" target="_blank" rel="noopener">' + escHtml(a.title) + '</a></span>' +
      '<span class="news-item-meta">' + (a.source ? '<span class="news-item-source">' + escHtml(a.source) + '</span>' : '') + (a.age ? ' · <span class="news-item-age">' + escHtml(a.age) + '</span>' : '') + '</span>' +
      '</div></li>';
  }

  function renderNewsSection(p) {
    if (!p.news_query) return '';
    var listContent = '';
    if (p.news_articles && p.news_articles.length > 0) {
      listContent = renderNewsArticle(p.news_articles[0]);
    } else {
      listContent = '<li class="news-item" style="color:var(--text-muted);font-size:0.73rem;">Loading news...</li>';
    }
    return '<div class="pair-news" data-news-query="' + escHtml(p.news_query) + '">' +
      '<div class="pair-news-header">' +
        '<span class="material-icons-round pair-news-icon">newspaper</span>' +
        '<span class="pair-news-title">Related News</span>' +
      '</div>' +
      '<ul class="news-list">' + listContent + '</ul>' +
      '<button class="news-see-more" onclick="loadMoreNews(this)">' +
        '<span class="material-icons-round" style="font-size:14px;vertical-align:middle;">add</span> See more' +
      '</button>' +
    '</div>';
  }

  function renderPairCard(p, idx) {
    return '<div class="pair-card ' + cardClass(p.confidence) + '" id="pair-' + idx + '" style="opacity:0;transform:translateY(16px);transition:opacity 400ms ease,transform 400ms ease;">' +
      '<div class="pair-head">' +
        '<div class="pair-index">' + (idx + 1) + '</div>' +
        '<div class="conf-badge ' + confClass(p.confidence) + '">' +
          '<span class="material-icons-round">' + confIcon(p.confidence) + '</span> ' +
          escHtml(p.confidence) +
        '</div>' +
        '<div class="head-separator"></div>' +
        '<div class="score-pills">' +
          '<div class="score-pill">Confidence <div class="bar-mini"><div class="bar-mini-fill" style="width:' + fmtScoreWidth(p.confidence_score) + '"></div></div> <strong>' + fmtScore(p.confidence_score) + '</strong></div>' +
        '</div>' +
        '<div class="head-spacer"></div>' +
        '<div class="route-chip">' +
          '<span class="material-icons-round">arrow_forward</span>' +
          '<span class="rv ' + venueClass(p.selected_venue) + '">' + escHtml(p.selected_venue) + '</span>' +
        '</div>' +
        '<button class="expand-btn" onclick="toggleExplain(this, \'explain-s' + idx + '\')">' +
          '<span class="material-icons-round">expand_more</span>' +
        '</button>' +
      '</div>' +
      '<div class="pair-body">' +
        renderMktCol(p.market_a) +
        renderMktCol(p.market_b) +
      '</div>' +
      renderNewsSection(p) +
      '<div class="pair-explain" id="explain-s' + idx + '">' +
        '<div class="pair-explain-inner">' +
          '<div class="pair-explain-section"><div class="pair-explain-label">Match reasoning</div><span class="pair-explain-text">' + escHtml(p.explanation) + '</span></div>' +
          '<div class="pair-explain-section">' +
            '<div class="pair-explain-label">Routing decision</div>' +
            '<div class="route-form" data-pair-idx="s' + idx + '"' +
              ' data-yes-a="' + (p.market_a.yes_price||0).toFixed(6) + '" data-no-a="' + (p.market_a.no_price||0).toFixed(6) + '"' +
              ' data-liq-a="' + (p.market_a.liquidity||0).toFixed(2) + '" data-spread-a="' + (p.market_a.spread||0).toFixed(6) + '"' +
              ' data-venue-a="' + escHtml(p.market_a.venue) + '"' +
              ' data-yes-b="' + (p.market_b.yes_price||0).toFixed(6) + '" data-no-b="' + (p.market_b.no_price||0).toFixed(6) + '"' +
              ' data-liq-b="' + (p.market_b.liquidity||0).toFixed(2) + '" data-spread-b="' + (p.market_b.spread||0).toFixed(6) + '"' +
              ' data-venue-b="' + escHtml(p.market_b.venue) + '">' +
              '<div class="route-form-row">' +
                '<label class="route-label">Side</label>' +
                '<div class="route-toggle">' +
                  '<button class="route-side-btn active" data-side="YES" onclick="setRouteSide(this,\'YES\')">YES</button>' +
                  '<button class="route-side-btn" data-side="NO" onclick="setRouteSide(this,\'NO\')">NO</button>' +
                '</div>' +
                '<label class="route-label">Size</label>' +
                '<div class="route-input-wrap"><span class="route-input-prefix">$</span><input type="number" class="route-size-input" value="1000" min="1" step="100" onchange="recalcRoute(this)"></div>' +
                '<button class="route-calc-btn" onclick="recalcRoute(this)"><span class="material-icons-round" style="font-size:14px">refresh</span> Calculate</button>' +
              '</div>' +
            '</div>' +
            '<div class="route-output" id="route-out-s' + idx + '"></div>' +
          '</div>' +
        '</div>' +
      '</div>' +
    '</div>';
  }

  function bindClickableMarkets(container) {
    container.querySelectorAll(".clickable-market").forEach(function(card) {
      card.addEventListener("click", function() {
        if (typeof showMarketModal === "function") showMarketModal(card);
      });
    });
    // Auto-fetch first news article for sections that have no articles yet
    container.querySelectorAll(".pair-news[data-news-query]").forEach(function(newsEl) {
      var list = newsEl.querySelector(".news-list");
      if (!list) return;
      // If there are already real articles, skip
      if (list.querySelector(".news-item a")) return;
      var query = newsEl.getAttribute("data-news-query");
      if (!query) return;
      list.innerHTML = '<li class="news-item" style="color:var(--text-muted);font-size:0.73rem;">Loading news...</li>';
      fetch("/news?q=" + encodeURIComponent(query))
        .then(function(r) { return r.json(); })
        .then(function(data) {
          var articles = data.articles || [];
          if (articles.length > 0) {
            list.innerHTML = renderNewsArticle(articles[0]);
          } else {
            list.innerHTML = '<li class="news-item" style="color:var(--text-muted);font-size:0.73rem;">No articles found</li>';
          }
        })
        .catch(function() {
          list.innerHTML = '';
        });
    });
  }

  function startStreamSearch(q, deepSearch) {
    showStreamUI(q);
    var target = "/?q=" + encodeURIComponent(q) + (deepSearch ? "&more=1" : "");
    history.pushState(null, "", target);
    var pairsContainer = document.getElementById("streamPairs");
    var matchCount = 0;
    var probableCount = 0;
    var pairCount = 0;

    var searchDone = false;
    function updateHeader() {
      var header = document.getElementById("streamHeader");
      if (!header) return;
      var html = '';
      if (searchDone) {
        html = '<div class="results-title">Results for <strong>"' + escHtml(q) + '"</strong></div>';
      } else if (pairCount > 0) {
        html = '<div class="results-title"><span class="material-icons-round" style="font-size:14px;vertical-align:middle;animation:loaderSpin 0.9s linear infinite;margin-right:4px;">sync</span> Finding matches for <strong>"' + escHtml(q) + '"</strong></div>';
      } else {
        html = '<div class="results-title"><span class="material-icons-round" style="font-size:14px;vertical-align:middle;animation:loaderSpin 0.9s linear infinite;margin-right:4px;">sync</span> Searching for <strong>"' + escHtml(q) + '"</strong></div>';
      }
      if (matchCount) html += '<span class="result-badge badge-match">' + matchCount + ' matched</span>';
      if (probableCount) html += '<span class="result-badge badge-probable">' + probableCount + ' probable</span>';
      header.innerHTML = html;
    }

    var es = new EventSource("/stream?q=" + encodeURIComponent(q) + (deepSearch ? "&more=1" : ""));
    es.onmessage = function(e) {
      try {
        var evt = JSON.parse(e.data);
        if (evt.type === "step") {
          addLine("loader-step", "spin", "↻", evt.msg, "");
        } else if (evt.type === "result") {
          var extra = venueTag(evt.venue);
          if (evt.count > 0) extra = countBadge(evt.count) + (evt.venue ? venueTag(evt.venue) : "");
          addLine("loader-result", "ok", "✓", evt.msg, extra);
        } else if (evt.type === "pair" && evt.pair) {
          var idx = pairCount;
          pairCount++;
          if (evt.pair.confidence === "MATCH") matchCount++;
          else if (evt.pair.confidence === "PROBABLE_MATCH") probableCount++;
          // Hide the loader log once the first pair arrives
          if (pairCount === 1) {
            var loader = document.getElementById("streamLoader");
            if (loader) loader.style.display = "none";
          }
          var cardHtml = renderPairCard(evt.pair, idx);
          var div = document.createElement("div");
          div.innerHTML = cardHtml;
          var card = div.firstChild;
          pairsContainer.appendChild(card);
          bindClickableMarkets(card);
          // Trigger initial route calculation for streamed pair
          var calcBtn = card.querySelector(".route-calc-btn");
          if (calcBtn) window.recalcRoute(calcBtn);
          requestAnimationFrame(function() {
            card.style.opacity = "1";
            card.style.transform = "translateY(0)";
          });
          updateHeader();
        } else if (evt.type === "done") {
          es.close();
          searchDone = true;
          var loader = document.getElementById("streamLoader");
          if (loader) {
            if (pairCount === 0) {
              // Move empty state above (replace loader area)
              loader.innerHTML =
                '<div class="empty-state">' +
                  '<div class="empty-icon material-icons-round">search_off</div>' +
                  '<div class="empty-title">No equivalent pairs found</div>' +
                  '<div class="empty-sub">Try a different search query.</div>' +
                '</div>';
            } else {
              loader.style.display = "none";
            }
          }
          updateHeader();
        } else if (evt.type === "error") {
          es.close();
          addLine("loader-result", "warn", "!", evt.msg, "");
        }
      } catch(ex) {}
    };
    es.onerror = function() {
      es.close();
      var loader = document.getElementById("streamLoader");
      if (loader && pairCount === 0) {
        loader.innerHTML =
          '<div class="empty-state">' +
            '<div class="empty-icon material-icons-round">error_outline</div>' +
            '<div class="empty-title">Connection lost</div>' +
            '<div class="empty-sub">The search was interrupted. Please try again.</div>' +
          '</div>';
      } else if (loader) {
        loader.style.display = "none";
      }
      updateHeader();
    };
  }

  // Forms and hero hints navigate naturally via GET /?q=...
  // for instant index results. startStreamSearch is kept global
  // for the "Search Live" button's onclick.
  window.startStreamSearch = startStreamSearch;

  // Handle browser back/forward: reload the page so the server renders the correct state.
  window.addEventListener("popstate", function() {
    window.location.reload();
  });

  // ─── Route form: client-side routing calculation ───────────────────────

  window.setRouteSide = function(btn, side) {
    var toggle = btn.parentElement;
    toggle.querySelectorAll(".route-side-btn").forEach(function(b) { b.classList.remove("active"); });
    btn.classList.add("active");
    recalcRoute(btn);
  };

  window.recalcRoute = function(el) {
    var form = el.closest(".route-form");
    if (!form) return;
    var idx = form.dataset.pairIdx;
    var output = document.getElementById("route-out-" + idx);
    if (!output) return;

    var side = "YES";
    var activeBtn = form.querySelector(".route-side-btn.active");
    if (activeBtn) side = activeBtn.dataset.side;

    var sizeInput = form.querySelector(".route-size-input");
    var size = parseFloat(sizeInput.value) || 1000;

    var d = form.dataset;
    var venueA = d.venueA || "A";
    var venueB = d.venueB || "B";
    var yesA = parseFloat(d.yesA) || 0;
    var yesB = parseFloat(d.yesB) || 0;
    var liqA = parseFloat(d.liqA) || 0;
    var liqB = parseFloat(d.liqB) || 0;
    var spreadA = parseFloat(d.spreadA) || 0;
    var spreadB = parseFloat(d.spreadB) || 0;

    var PW = 0.60, LW = 0.30, SW = 0.10;

    function scoreVenue(yes, liq, spread) {
      var price = (side === "YES") ? (1.0 - yes) : yes;
      var liquidity = Math.tanh(liq / size);
      var sp = (spread === 0) ? 0.5 : (1.0 - Math.min(spread / 0.20, 1.0));
      return { price: price, liquidity: liquidity, spread: sp, total: PW*price + LW*liquidity + SW*sp };
    }

    var sA = scoreVenue(yesA, liqA, spreadA);
    var sB = scoreVenue(yesB, liqB, spreadB);
    var aWins = sA.total >= sB.total;
    var winner = aWins ? venueA : venueB;
    var loseVenue = aWins ? venueB : venueA;
    var winScore = aWins ? sA : sB;
    var loseScore = aWins ? sB : sA;
    var winYes = aWins ? yesA : yesB;
    var winLiq = aWins ? liqA : liqB;
    var winSpread = aWins ? spreadA : spreadB;
    var loseLiq = aWins ? liqB : liqA;
    var loseSpread = aWins ? spreadB : spreadA;

    var costWin = (side === "YES") ? winYes : (1 - winYes);
    var costLose = (side === "YES") ? (aWins ? yesB : yesA) : (1 - (aWins ? yesB : yesA));
    var sharesWin = (costWin > 0) ? (size / costWin) : 0;
    var payout = sharesWin * 1.0;
    var profit = payout - size;
    var ret = (size > 0) ? ((profit / size) * 100) : 0;

    function fmtBps(s) { return (s * 10000).toFixed(0); }
    function fmtK(n) { if (n >= 1e6) return "$" + (n/1e6).toFixed(1) + "M"; if (n >= 1e3) return "$" + (n/1e3).toFixed(1) + "K"; return "$" + Math.round(n); }

    function venueCard(name, s, yes, liq, spread, isWinner) {
      var cost = (side === "YES") ? yes : (1 - yes);
      var shares = (cost > 0) ? (size / cost) : 0;
      var cls = isWinner ? "rv-card rv-winner" : "rv-card";
      var badge = isWinner ? '<span class="rv-badge">BEST</span>' : '';
      var liqWarn = (liq < size) ? ' <span class="rv-warn">(' + Math.round((liq/size)*100) + '% fill)</span>' : '';
      var spreadLabel = "";
      if (spread === 0) { spreadLabel = "N/A"; }
      else { spreadLabel = fmtBps(spread) + " bps"; }
      return '<div class="' + cls + '">' +
        '<div class="rv-head"><span class="rv-venue">' + escHtml(name) + '</span>' + badge + '<span class="rv-score">' + s.total.toFixed(3) + '</span></div>' +
        '<div class="rv-stats">' +
          '<div class="rv-stat"><div class="rv-stat-label">Price</div><div class="rv-stat-val">$' + cost.toFixed(4) + '</div></div>' +
          '<div class="rv-stat"><div class="rv-stat-label">Shares</div><div class="rv-stat-val">~' + Math.round(shares).toLocaleString() + '</div></div>' +
          '<div class="rv-stat"><div class="rv-stat-label">Liquidity</div><div class="rv-stat-val">' + fmtK(liq) + liqWarn + '</div></div>' +
          '<div class="rv-stat"><div class="rv-stat-label">Spread</div><div class="rv-stat-val">' + spreadLabel + '</div></div>' +
        '</div>' +
      '</div>';
    }

    // Reasons
    var reasons = [];
    if (winScore.price > loseScore.price && costLose > 0) {
      var pctDiff = ((costLose - costWin) / costLose * 100).toFixed(1);
      reasons.push("Better price: $" + costWin.toFixed(4) + " vs $" + costLose.toFixed(4) + " on " + escHtml(loseVenue) + " (" + pctDiff + "% cheaper)");
    }
    if (winLiq > loseLiq * 2) {
      reasons.push("Much deeper liquidity: " + fmtK(winLiq) + " vs " + fmtK(loseLiq) + " on " + escHtml(loseVenue));
    } else if (winLiq > loseLiq) {
      reasons.push("More liquidity: " + fmtK(winLiq) + " vs " + fmtK(loseLiq) + " on " + escHtml(loseVenue));
    }
    if (winScore.spread > loseScore.spread && winSpread > 0) {
      reasons.push("Tighter spread: " + fmtBps(winSpread) + " bps vs " + fmtBps(loseSpread) + " bps on " + escHtml(loseVenue));
    }
    if (reasons.length === 0) reasons.push("Higher overall weighted score");

    var reasonsHtml = '<ul class="rv-reasons">';
    for (var i = 0; i < reasons.length; i++) {
      reasonsHtml += '<li>' + reasons[i] + '</li>';
    }
    reasonsHtml += '</ul>';

    var html = '<div class="rv-grid">';
    if (aWins) {
      html += venueCard(venueA, sA, yesA, liqA, spreadA, true);
      html += venueCard(venueB, sB, yesB, liqB, spreadB, false);
    } else {
      html += venueCard(venueB, sB, yesB, liqB, spreadB, true);
      html += venueCard(venueA, sA, yesA, liqA, spreadA, false);
    }
    html += '</div>';

    html += '<div class="rv-why"><div class="rv-why-title">Why ' + escHtml(winner) + '?</div>' + reasonsHtml + '</div>';

    html += '<div class="rv-exec">' +
      '<div class="rv-exec-title">Estimated Execution</div>' +
      '<div class="rv-exec-grid">' +
        '<div class="rv-exec-item"><span class="rv-exec-label">Venue</span><span class="rv-exec-val">' + escHtml(winner) + '</span></div>' +
        '<div class="rv-exec-item"><span class="rv-exec-label">Side</span><span class="rv-exec-val">BUY ' + side + '</span></div>' +
        '<div class="rv-exec-item"><span class="rv-exec-label">Cost/share</span><span class="rv-exec-val">$' + costWin.toFixed(4) + '</span></div>' +
        '<div class="rv-exec-item"><span class="rv-exec-label">Order size</span><span class="rv-exec-val">' + fmtK(size) + '</span></div>' +
        '<div class="rv-exec-item"><span class="rv-exec-label">Shares</span><span class="rv-exec-val">~' + Math.round(sharesWin).toLocaleString() + '</span></div>' +
        '<div class="rv-exec-item rv-exec-highlight"><span class="rv-exec-label">If correct</span><span class="rv-exec-val">' + fmtK(payout) + ' payout (' + fmtK(profit) + ' profit, ' + Math.round(ret) + '% return)</span></div>' +
      '</div>' +
    '</div>';

    output.innerHTML = html;
  };

})();
