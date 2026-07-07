# Roadmap (по фазам)

Принцип: каждая фаза — вертикальный срез, который можно запустить и **увидеть результат**.
Не строим горизонтальные слои «впрок». Простое → сложное. Откладываем всё из раздела
«Сознательно отложено» в `ARCHITECTURE.md`.

---

## Phase 0 — Bootstrap — done

**Цель:** доказать сквозную связку «наш Go-код → `idb_companion` → реальные симуляторы».
Это «hello world», который проверяет всю фундаментальную цепочку.

**Deliverables:**
1. `go.mod` инициализирован.
2. Проверка окружения: `idb_companion` найден в PATH, печатаем его версию (`idb_companion --version`).
3. Пакет `internal/companion`:
   - функция, которая запускает `idb_companion --list 1`, парсит JSON-вывод в `[]Simulator`
     (UDID, name, OS version, state).
4. `cmd/simcastd` с подкомандой `list`, которая печатает доступные симуляторы пользователя.
5. README обновлён инструкцией запуска.

**Definition of Done:**
```
go run ./cmd/simcastd list
```
печатает РЕАЛЬНЫЙ список симуляторов с машины (UDID + имя + версия iOS + booted/shutdown).
Никаких заглушек — данные из настоящего `idb_companion`.

**Подсказки агенту:**
- `idb.proto` можно скопировать из `~/Developer/ios-bridge/.venv/proto/idb.proto`.
- companion уже стоит: `/opt/homebrew/bin/idb_companion` (v1.1.8).
- Полный список флагов: `idb_companion --help`.
- На этом этапе gRPC ещё НЕ нужен — `--list` это CLI-вывод. gRPC заводим в Phase 1.

---

## Phase 1 — JPEG MVP (видно симулятор на «клиенте») — done

**Цель:** самый дешёвый сквозной стрим без WebRTC, чтобы увидеть картинку и проверить ввод.

- gRPC-стабы из `idb.proto` (grpc-go), подключение к `idb_companion --udid <UDID> --grpc-port N`.
- `describe` → размер экрана.
- Кадры: либо `video_stream`, либо периодический `screenshot` → раздаём по WebSocket как JPEG.
- `hid`: реализовать `tap(x,y)` (down+up) и кнопку `Home`.
- Простейший клиент-заглушка (даже веб-страница / маленькое Swift-приложение): рисует кадры,
  по клику шлёт tap с маппингом координат.

**DoD:** на «клиенте» видно живой экран симулятора, клик проходит как тап в симулятор.

---

## Phase 2 — WebRTC (низкая задержка) — done

- pion: `TrackLocalStaticSample`, H.264 из `video_stream` в трек без энкодинга.
- Signaling (минимальный WSS): offer/answer/ICE.
- Тачи → WebRTC DataChannel (unreliable/unordered).
- Swipe + остальные кнопки в `hid`.

**DoD:** низколатентный H.264-поток на клиента по WebRTC, тачи по DataChannel.

---

## Phase 3 — Удалёнка / рандеву-сервер — done

Полный дизайн — `docs/superpowers/specs/2026-06-03-phase4-remote-access-design.md`
(имя файла историческое — это контент нынешней Phase 3).

- **Signaling-сервер на Go** (`cmd/simcast-signal`): WSS-брокер «комнат» по `pairingToken`,
  релей offer/answer + ICE, выписка короткоживущих TURN-кредов (HMAC).
- **Транспортные изменения демона:** control- и видео-плоскости разведены. PeerConnection +
  control-DataChannel поднимаются на пейринге (per-client-session, без UDID); list/boot/describe
  и тачи — по DataChannel; видео-трек добавляется on-demand по `attach <udid>`, снимается по
  `detach`. Демон делает только исходящее — ноль открытых портов.
- **Pairing:** `pairingToken` + `daemonPubKey` (в браузере — через URL; QR — для нативного
  клиента позже). Pubkey аутентифицирует рукопожатие (анти-MITM).
- **STUN/TURN gating:** STUN всем; TURN только «подписчикам» (в этой фазе — стаб проверки);
  free при `iceConnectionState=failed` → апселл. TURN-движок — готовый `coturn`, свой не пишем.
- **Валидация — в браузере** (расширение Phase 2 `/rtc`-клиента). Нативный клиент НЕ нужен.

**DoD:** в браузере через интернет (или эмуляцию сети) — пейринг по токену, список симуляторов
и бут выключенного по DataChannel, выбор симулятора → видео по WebRTC, тачи по DataChannel;
в одной сети P2P идёт напрямую (host), TURN не задействован.

### Phase 3C — done

Постоянная парность (спарились ключами один раз → реконнект по `daemonID` без QR),
аккаунты-по-ключам, подписки (`POST /v1/subscription`, две подписи, SQLite за
`Store`), гейт TURN по реальной подписке (стаб `--grant-turn` убран). Дизайн —
`docs/superpowers/specs/2026-06-04-phase3c-identity-accounts-design.md`; план —
`docs/superpowers/plans/2026-06-04-phase3c-identity-accounts.md`. Решения #55–#64.
**Phase 4** несёт: серверную проверку чека Apple (флип `source`), TLS/домен,
Homebrew-дистрибуцию, Postgres (через `Store`).

---

## Phase 4 — Дистрибуция + self-host — done

- **GoReleaser**: один пайплайн собирает `simcastd` (darwin arm64/amd64) и
  `simcast-signal` (linux amd64) на тег `v*`.
- **Homebrew tap** (`kei-sidorov/homebrew-simcast`): предсобранный неподписанный
  `simcastd`, зависимости `idb-companion` + `ffmpeg`. Обновление — `brew upgrade`.
- **Self-host сервера**: VPS + systemd, брокер + coturn за Caddy (авто-TLS).
  **Pull-автообновление**: systemd timer тянет новый релиз из GitHub Releases,
  проверяет checksum, атомарно подменяет бинарь, рестартит юнит. Ноль серверных
  секретов в репо/CI; deploy-скаффолдинг генерик, секреты — на сервере.
- **Серверную проверку чека Apple НЕ делаем** в этой фазе — подписки остаются
  client-asserted (решение #62). Дизайн — `docs/superpowers/specs/2026-06-06-phase4-distribution-design.md`.

---

## Phase 5 — Demo backend (App Review / try-before-you-buy) — done

Проблема: ревьюеру Apple (и любому «просто посмотреть») нужен работающий
endpoint без Mac-сетапа. Решение — интерактивное демо-устройство из headless-браузера.

- **Рефакторинг**: устройство за интерфейсами `server.Backend`/`server.Feed`;
  вся idb-механика уехала в `internal/backend/sim`. Сессионный слой бэкенда не знает.
- **`internal/backend/browser`**: headless Chromium (chromedp) с mobile-эмуляцией;
  скриншот-полл → тот же ffmpeg-пайплайн; tap/swipe/key → CDP-события; Home → стартовый URL.
- **encoder**: `h264_videotoolbox` на darwin, `libx264` (ultrafast+zerolatency) на остальных.
- **`simcastd demo`**: unattended-режим — pairing-окно с фиксированным секретом,
  перевзводится после каждого энролла (multi-use URL для App Review notes).
- **Дистрибуция**: GoReleaser собирает `simcastd` и под linux (amd64/arm64);
  `deploy/systemd/simcastd-demo.service` + `demo.env.example`; cask на Mac не тронут.

**DoD (выполнено):** локальный брокер + `simcastd demo` + три реальных Chrome-клиента
через web/debug — энролл по одному URL, attach, декодированное видео 390×844 по WebRTC.

---

### Вне скоупа этого репозитория
**Нативный iPad-клиент** (Swift + GoogleWebRTC, рендер трека, жесты, сканер QR, маппинг
координат/повороты/letterboxing) делается **в отдельном репозитории** и **только когда серверная
часть здесь готова и проверена в браузере**. Это платный продукт; здесь — open-source сервер.

### Что НЕ делать раньше времени
Адаптивный битрейт, свой ScreenCaptureKit-пайплайн, бандлинг companion'а, нотаризация GUI,
реальные устройства — см. «Сознательно отложено» в `ARCHITECTURE.md`.
