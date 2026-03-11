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
  ["mdTitle","mdVenue","mdMarketId","mdStatus","mdDescription","mdTags",
   "mdCategory","mdResolutionDate","mdResolutionCriteria","mdYes",
   "mdLiquidity","mdSpread","mdVolume",
   "mdOpenInterest","mdRawPayload","mdLinks","mdImage","mdImageBanner",
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

  window.showMarketModal = function(card) {
    var d = card.dataset;
    fields.mdTitle.textContent = safe(d.title);
    fields.mdVenue.textContent = safe(d.venue);
    fields.mdMarketId.textContent = safe(d.marketId);
    fields.mdStatus.textContent = safe(d.status);
    fields.mdDescription.textContent = safe(d.description);
    fields.mdTags.textContent = safe(d.tags);
    fields.mdCategory.textContent = safe(d.category);
    fields.mdResolutionDate.textContent = safe(d.resolutionDate);
    fields.mdResolutionCriteria.textContent = safe(d.resolutionCriteria);
    fields.mdVolume.textContent = safe(d.volume24h);
    fields.mdOpenInterest.textContent = safe(d.openInterest);
    fields.mdYes.textContent = safe(d.yes);
    fields.mdLiquidity.textContent = safe(d.liquidity);
    fields.mdSpread.textContent = safe(d.spread);

    activeLive.venue = String(d.venue || "").toLowerCase();
    activeLive.marketId = String(d.marketId || "");
    var initYes = parseFloat(d.yes);
    if (window.__livePriceState && isFinite(initYes)) {
      window.__livePriceState.publish(activeLive.venue, activeLive.marketId, initYes);
    }
    renderLivePrice(activeLive.venue, activeLive.marketId);

    var imgUrl = d.imageUrl || "";
    if (imgUrl) {
      fields.mdImage.src = imgUrl;
      fields.mdImageBanner.style.display = "block";
    } else {
      fields.mdImage.src = "";
      fields.mdImageBanner.style.display = "none";
    }

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
      links.innerHTML += '<a class="modal-link" href="' + venueLink + '" target="_blank"><span class="material-icons-round">open_in_new</span>Open on ' + safe(d.venue) + '</a>';
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
    return '<span class="loader-venue-' + venue + '">' + venue + '</span>';
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
        '<span class="mkt-price" data-venue="' + escHtml(m.venue) + '" data-market-id="' + escHtml(m.venue_market_id) + '" data-token-id="' + escHtml(m.venue_yes_token_id || "") + '">' + fmtPct(m.yes_price || 0) + '</span>' +
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
          '<div class="score-pill">Fuzzy <div class="bar-mini"><div class="bar-mini-fill" style="width:' + fmtScoreWidth(p.fuzzy_score) + '"></div></div> <strong>' + fmtScore(p.fuzzy_score) + '</strong></div>' +
          '<div class="score-pill">Composite <div class="bar-mini"><div class="bar-mini-fill" style="width:' + fmtScoreWidth(p.composite_score) + '"></div></div> <strong>' + fmtScore(p.composite_score) + '</strong></div>' +
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
          '<div class="pair-explain-section"><div class="pair-explain-label">Match reasoning</div>' + escHtml(p.explanation) + '</div>' +
          '<div class="pair-explain-section"><div class="pair-explain-label">Routing decision</div>' + escHtml(p.routing_reason) + '</div>' +
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
                  '<div class="empty-sub">Try a different search query, or adjust MATCH_THRESHOLD / MAX_DATE_DELTA_DAYS to widen the match window.</div>' +
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

  document.querySelectorAll("form").forEach(function(form) {
    form.addEventListener("submit", function(e) {
      var input = form.querySelector("[name=q]");
      if (!input) return;
      var q = input.value.trim();
      if (!q) return;
      e.preventDefault();
      startStreamSearch(q, false);
    });
  });

  // Intercept hint links (they navigate directly, skip them to stay SSE-driven)
  document.querySelectorAll(".hero-hint").forEach(function(a) {
    a.addEventListener("click", function(e) {
      e.preventDefault();
      var url = new URL(a.href);
      var q = url.searchParams.get("q") || "";
      if (q) startStreamSearch(q, false);
    });
  });

})();
