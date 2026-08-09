# simcast → simbeam: миграция прод-стека

Одноразовый runbook. Репозиторий уже целиком переименован (`main`). Здесь —
что доделать вне git: GitHub-релиз/tap и живой VPS. Домен `simbeam.sidorov.tech`
уже создан пользователем.

**Инварианты после переименования (для сверки):**
- Модуль `github.com/kei-sidorov/simbeam`, репо то же.
- Бинари: `simbeamd`, `simbeam-signal`. Юниты: `simbeam-signal`, `simbeamd-demo`,
  `simbeam-signal-update.{service,timer}`.
- Пути: `/usr/local/bin/simbeam*`, `/etc/simbeam/`, `/var/lib/simbeam/`, user `simbeam`.
- Апдейтер тянет `simbeam-signal_<v>_linux_amd64.tar.gz` из `kei-sidorov/simbeam`.
- goreleaser пушит cask `simbeamd` в tap-репо `kei-sidorov/homebrew-simbeam`.
- **Env-имена остались `SIMCAST_*`** (SIMCAST_APP_SECRET / SIMCAST_SIGNAL_ARGS /
  SIMCAST_DEMO_ARGS / SIMCAST_PAIR_SECRET). Не переименовывать — код читает их же.
- Забайканный в НОВЫЕ бинари `defaultSignalURL = wss://simbeam.sidorov.tech/ws`.
  В уже отгруженных v0.2.1 клиентах забайкан **старый** `wss://simcast.sidorov.tech/ws`.

---

## Порядок: сперва релиз, потом сервер

Апдейтер и bootstrap тянут артефакты по НОВЫМ именам — их сначала нужно выпустить.

## Фаза A — GitHub / Homebrew (с ноутбука)

- [ ] **A1. Tap-репо.** Переименовать на GitHub `kei-sidorov/homebrew-simcast` →
      `homebrew-simbeam` (Settings → Rename). GitHub оставит redirect.
- [ ] **A2. Токен.** Проверить, что секрет `HOMEBREW_TAP_TOKEN` в Actions репо
      `simbeam` жив и имеет доступ к `homebrew-simbeam` (classic PAT со `repo`
      scope переживает переименование; fine-grained — проверить, что грант
      привязан к новому имени/репо).
- [ ] **A3. Remote локального чекаута** уже обновлён на `…/simbeam.git` (сделано).
- [ ] **A4. Выпустить релиз.** Тег новой минорной версии (например `v0.3.0`),
      пуш тега → `release.yml` соберёт `simbeam*`-архивы + запушит cask
      `simbeamd` в `homebrew-simbeam`.
      Проверить: в GitHub Release лежат `simbeamd_<v>_darwin_{arm64,amd64}.tar.gz`,
      `simbeamd_<v>_linux_{amd64,arm64}.tar.gz`, `simbeam-signal_<v>_linux_amd64.tar.gz`,
      `checksums.txt`; в tap появился `Casks/simbeamd.rb`.
- [ ] **A5. (опц.) Прибрать старый tap:** удалить `Casks/simcastd.rb` из
      `homebrew-simbeam`, чтобы старое имя не резолвилось.
- [ ] **A6. Инструкция для существующих macOS-юзеров** (старое имя ломается):
      ```
      brew uninstall simcastd || true
      brew untap kei-sidorov/simcast || true
      brew install kei-sidorov/simbeam/simbeamd
      ```

## Фаза B — DNS / Caddy (с сохранением обратной совместимости)

Уже отгруженные v0.2.1-клиенты стучатся на **старый** `simcast.sidorov.tech`.
Пока они не обновятся — старый хост должен продолжать работать.

- [ ] **B1. DNS.** Убедиться, что A-запись `simbeam.sidorov.tech → 45.76.90.247`
      уже резолвится: `dig +short simbeam.sidorov.tech`.
- [ ] **B2. Caddy.** Обслуживать ОБА хоста на тот же брокер (127.0.0.1:9000).
      В реальном Caddyfile на боксе:
      ```
      simbeam.sidorov.tech { reverse_proxy 127.0.0.1:9000 }
      simcast.sidorov.tech { reverse_proxy 127.0.0.1:9000 }   # legacy, снять после миграции клиентов
      ```
      `systemctl reload caddy` → дождаться нового LE-сертификата на simbeam-хост.

## Фаза C — VPS in-place миграция (SSH: `nanoclaw@45.76.90.247`, sudo)

Стратегия: остановить старое, перенести секреты/стейт под новые имена, накатить
новые юниты через `bootstrap.sh`, стартовать, проверить, старое убрать **после** соака.

- [ ] **C1. Стоп старого.**
      ```
      sudo systemctl disable --now simcast-signal.service simcastd-demo.service \
        simcast-signal-update.timer simcast-signal-update.service
      ```
- [ ] **C2. Перенести секреты/env.**
      ```
      sudo install -d -m 0750 /etc/simbeam
      sudo cp /etc/simcast/signal.env /etc/simbeam/signal.env
      sudo cp /etc/simcast/demo.env   /etc/simbeam/demo.env
      ```
      Отредактировать оба: заменить `/var/lib/simcast`→`/var/lib/simbeam`,
      `simcast.db`→`simbeam.db`, и в demo.env `wss://simcast.sidorov.tech/ws`→
      `wss://simbeam.sidorov.tech/ws`. Значение `--turn turn:<host>:3478` —
      оставить хост, который резолвится (можно IP или любой из доменов).
      Проверить владельца/права: `sudo chmod 600 /etc/simbeam/*.env`.
- [ ] **C3. Перенести стейт.** Скопировать (не move — пусть старое останется для
      отката), затем чоунить на нового юзера (создастся в C4):
      ```
      sudo rsync -a /var/lib/simcast/ /var/lib/simbeam/
      # переименовать sqlite брокера под новый --db путь
      sudo mv /var/lib/simbeam/simcast.db /var/lib/simbeam/simbeam.db 2>/dev/null || true
      ```
      В `/var/lib/simbeam/` должны оказаться: `simbeam.db`, `demo-identity.key`,
      `demo-clients.json`, каталог `demo/` (с `index.html`).
      **Сохранение `demo-identity.key` + того же `SIMCAST_PAIR_SECRET` = тот же
      многоразовый pairing-URL** (важно, если он уже в App Review notes).
- [ ] **C4. Обновить чекаут репо на боксе и накатить юниты.**
      ```
      cd <checkout>            # там, где deploy/ (git remote → simbeam.git)
      git pull
      sudo chown -R simbeam:simbeam /var/lib/simbeam   # (bootstrap создаст юзера, если нет)
      sudo ./deploy/bootstrap.sh
      ```
      bootstrap идемпотентен: создаст user `simbeam`, положит новые unit-файлы +
      `/usr/local/bin/simbeam-signal-update.sh`, дёрнет первый pull бинаря
      `simbeam-signal`, включит broker + timer. `/etc/simbeam/signal.env` НЕ
      перезапишет (он уже есть из C2).
- [ ] **C5. Demo-бинарь (авто-апдейтера у него нет — руками).** Скачать linux
      `simbeamd` из релиза A4 и положить рядом:
      ```
      ARCH=amd64; V=<version_without_v>
      curl -fsSL -o /tmp/d.tgz \
        https://github.com/kei-sidorov/simbeam/releases/download/v$V/simbeamd_${V}_linux_${ARCH}.tar.gz
      tar -xzf /tmp/d.tgz -C /tmp && sudo install -m0755 /tmp/simbeamd /usr/local/bin/simbeamd
      sudo systemctl enable --now simbeamd-demo.service
      ```
- [ ] **C6. Firewall (ufw в bootstrap НЕ входит — всегда вручную).** Правила
      port-based, переименование их не трогает; просто проверить, что живы:
      ```
      sudo ufw status | grep -E '3478|49152:65535|32768:60999'
      ```
      Если нет — добавить: `3478/udp` (coturn), `49152:65535/udp` (relay),
      `32768:60999/udp` (pion host-кандидаты).
- [ ] **C7. coturn.** Файл `/etc/turnserver.conf` не связан с именем проекта;
      `static-auth-secret` должен по-прежнему совпадать с `--turn-secret` в
      signal.env. `external-ip`/`realm` не трогаем. Рестарт не обязателен.

## Фаза D — Проверка

- [ ] **D1.** `simbeam-signal --version` = версия релиза; `systemctl status
      simbeam-signal simbeamd-demo simbeam-signal-update.timer` — все active.
- [ ] **D2.** Логи чистые: `journalctl -u simbeam-signal -n50`,
      `journalctl -u simbeamd-demo -n50`.
- [ ] **D3.** Pairing/attach через `wss://simbeam.sidorov.tech/ws` (веб-дебаг или
      iOS): pair → attach → декод H.264 → тап проходит в игру.
- [ ] **D4.** Забрать актуальный demo pairing-URL для App Review notes:
      `journalctl -u simbeamd-demo | grep -A3 "Pairing URL"`.
- [ ] **D5.** Апдейтер: `sudo /usr/local/bin/simbeam-signal-update.sh --dry-run`
      → «up to date».
- [ ] **D6.** Legacy: старый `wss://simcast.sidorov.tech/ws` всё ещё проксирует на
      брокер (для не обновлённых v0.2.1-клиентов).

## Фаза E — Уборка (после соака, когда новый стек стабилен)

- [ ] **E1.** Убрать старые юнит-файлы: `sudo rm /etc/systemd/system/simcast-*.service
      /etc/systemd/system/simcast-*.timer && sudo systemctl daemon-reload`.
- [ ] **E2.** Старый апдейтер и бинари: `sudo rm -f /usr/local/bin/simcast-signal-update.sh
      /usr/local/bin/simcastd /usr/local/bin/simcast-signal`.
- [ ] **E3.** Старый стейт/конфиг: `sudo rm -rf /etc/simcast /var/lib/simcast`.
- [ ] **E4.** Старый юзер: `sudo userdel simcast` (если создавался отдельный).
- [ ] **E5.** Когда все клиенты обновлены на v0.3.0+ (бьют в simbeam-хост) —
      снять legacy-блок `simcast.sidorov.tech` из Caddyfile, reload. Опц. снять
      DNS-запись старого хоста.

---

### Точки, где легко споткнуться
- **Порядок A→C обязателен:** bootstrap C4 и апдейтер D5 тянут `simbeam-signal_*`
  из релиза — без A4 качать нечего.
- **Не убивай `simcast.sidorov.tech` раньше времени** — в отгруженных v0.2.1
  бинарях этот адрес забайкан; смерть хоста = мёртвые существующие установки.
- **Апдейтер сам себя не обновляет** — новый `simbeam-signal-update.sh` кладёт
  bootstrap (C4); старый таймер надо погасить (C1), иначе два таймера.
- **demo-identity.key + тот же SIMCAST_PAIR_SECRET** держат pairing-URL стабильным;
  потеряешь ключ — URL сменится, App Review notes протухнут.
