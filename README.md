# simcast

**Стриминг iOS-симулятора с Mac на iPad для удалённой разработки.**

Запускаешь iOS Simulator на Mac → видишь и управляешь им с iPad. Тапы, свайпы, кнопки
(Home и т.д.) проксируются обратно на симулятор. Цель — комфортно разрабатывать под iOS,
когда сам сидишь за iPad, а симулятор крутится на удалённом/настольном Mac.

> ⚠️ Только **симуляторы**. Реальные устройства — НЕ цель проекта (см. `docs/ARCHITECTURE.md`).

---

## Что именно делаем

- **Сервер (демон)** на Mac — open-source. Поднимает симулятор, отдаёт видеопоток, принимает тачи.
- **Клиент** на iPad — отдельный (в перспективе платный) продукт. Принимает поток, рисует, шлёт жесты.
- Демон **не переписывает движок** — всю тяжёлую работу с CoreSimulator и видео делает
  `idb_companion` (нативный бинарь от Meta, MIT). Наш демон — тонкая обвязка над его gRPC.

## Стек (и почему)

| Часть | Технология | Почему |
|-------|-----------|--------|
| Движок симулятора | **`idb_companion`** (Meta, MIT) | готовый, нативный, делает CoreSimulator + видео; ставится из brew |
| Демон (macOS) | **Go** | после того как видео отдали idb, демон = «gRPC внутрь → WebRTC наружу» — вотчина Go; один статический бинарь |
| Энкодер | **ffmpeg** / `h264_videotoolbox` | аппаратный H.264-энкод PNG-кадров; мы владеем GOP и keyframe'ами (короткий ~1–2с), idb `video_stream` — нет (фиксированный GOP ~10с, без управления — см. решения №34–35) |
| WebRTC (сервер) | **pion** | лучший серверный WebRTC; отдаёт H.264-трек, который мы производим через ffmpeg |
| Транспорт к companion | **gRPC** (`grpc-go`, стабы из `idb.proto`) | родной API idb; нужны буквально `describe` + `screenshot` + `hid` |
| Клиент (iPad) | **Swift + GoogleWebRTC** | приёмная сторона WebRTC на iOS — хорошо проторённый путь |
| Дистрибуция демона | **Homebrew tap + GoReleaser** | prebuilt бинари, без нотаризации (brew не вешает quarantine), `depends_on idb-companion` |

Подробное обоснование всех решений — в `docs/ARCHITECTURE.md`.
План по фазам — в `docs/ROADMAP.md`.

## Минимальная нужная RPC-поверхность

Из всего `idb.proto` нам нужно по сути 3 вызова:

- `describe` — размер экрана симулятора (для маппинга координат тача);
- `screenshot` (unary) — PNG-кадр экрана, который мы кодируем в H.264 через ffmpeg;
- `hid` (streaming) — тачи/свайпы/кнопки внутрь (tap = touch down + up, swipe = down + moves + up, Home = button event).

Lifecycle (список, бут) — это **не gRPC**, а CLI-флаги самого companion'а:
`idb_companion --list 1`, `idb_companion --boot <UDID>`, `idb_companion --udid <UDID> --grpc-port <N>`.

## Архитектура (одной схемой)

```
iPad / браузер (WebRTC)
   │  видео ◄── WebRTC (H.264-трек) ────────────┐
   │  тачи  ──► DataChannel (JSON-команды) ──────┐│
   ▼                                             ▼▼
Mac: simcastd (Go)
   ├─ spawn  → idb_companion --udid <UDID> --grpc-port N   (сайдкар-процесс)
   ├─ gRPC   → describe / screenshot / hid
   ├─ ffmpeg → screenshot (PNG) → h264_videotoolbox → H.264 Annex-B
   └─ pion   → WebRTC-трек (H.264) + DataChannel (control)
                    │
                    ▼
              iOS Simulator (CoreSimulator, нужен Xcode)
```

## Раскладка репозитория (целевая)

```
simcast/
├── README.md                  # этот файл
├── CLAUDE.md                  # инструкции для агента + Definition of Done по фазам
├── docs/
│   ├── ARCHITECTURE.md        # решения и их обоснование (контекст всех обсуждений)
│   ├── ROADMAP.md             # фазы: 0 bootstrap → 1 JPEG → 2 WebRTC → 3 удалёнка → 4 дистрибуция
│   └── decisions.md           # короткий лог принятых решений (ADR-lite)
├── cmd/simcastd/              # точка входа демона        (создаётся в Bootstrap)
├── internal/companion/        # обвязка над idb_companion  (создаётся в Bootstrap)
└── go.mod                     # (создаётся в Bootstrap)
```

## Предпосылки на машине

- macOS + **Xcode / Command Line Tools** (без них нет симуляторов — `idb_companion` это не заменит).
- **`idb_companion`**: `brew install idb-companion` (уже стоит у автора в `/opt/homebrew/bin/idb_companion`, v1.1.8).
- **`ffmpeg`** с энкодером `h264_videotoolbox`: `brew install ffmpeg` (нужен для WebRTC-режима — кодирует кадры в H.264).
- **Go** (для разработки демона).

## Install (Homebrew)

Install the macOS daemon from the tap (pulls `idb-companion` and `ffmpeg` as deps):

```bash
brew install kei-sidorov/simcast/simcastd
simcastd version
```

Update later with `brew upgrade`. To self-host the signalling server (broker + TURN
with auto-update), see `deploy/README.md`.

## Запуск

Нужен установленный Go (`brew install go`) и `idb_companion` в PATH.

```bash
go run ./cmd/simcastd list
```

Печатает реальный список симуляторов с машины — состояние, имя, версию iOS и UDID,
например:

```
idb_companion: /opt/homebrew/bin/idb_companion (built Aug 12 2022 08:41:50)

STATE     NAME           OS        UDID
Booted    iPhone 17 Pro  iOS 26.4  6B0C54AC-4629-42FA-B9DA-ABBC39EF2027
Shutdown  iPhone 14      iOS 16.0  900DA5DC-267C-44C9-ADF3-23DA510111F9
...

13 simulator(s).
```

Данные берутся из настоящего `idb_companion --list 1` (не заглушка). Реальные устройства
отфильтровываются — скоуп проекта только симуляторы.

## Запуск стрима (WebRTC)

Стрим экрана идёт **только по аутентифицированному WebRTC-пути** через signaling-брокер:
видео, управление и сам `list`/`boot` симуляторов получает лишь спаренный клиент. Незащищённых
эндпоинтов нет — старые `WS /session` (JPEG-поллинг), dev `WS /rtc` (offer/answer без auth) и
REST `/api/{simulators,boot}` удалены (`docs/decisions.md` №75). Как поднять брокер, демон и
спарить браузер — см. [Удалёнка / рандеву](#удалёнка--рандеву-phase-3b) и
[Remote pairing](#remote-pairing-phase-3c).

HTTP-сервер демона теперь раздаёт **только** статику дебаг-клиента при `--web`; без него по HTTP
не отдаётся ничего. Список симуляторов и буут идут по control-DataChannel:

**Пайплайн видео** (после `attach <udid>` по DataChannel):

```
idb screenshot (PNG-поллинг) → ffmpeg (h264_videotoolbox, короткий GOP ~1–2с,
  половинное разрешение — браузер апскейлит) → pion → <video>
```

Управление (тачи, свайпы, Home, физическая клавиатура) идёт по **WebRTC DataChannel** тем же
JSON-форматом, координаты нормализованы [0,1] от показанного кадра:

- `{"type":"tap","x":..,"y":..}`
- `{"type":"swipe","x1":..,"y1":..,"x2":..,"y2":..,"duration":<сек>}`
- `{"type":"home"}`
- `{"type":"key","key":"<KeyboardEvent.key>"}` (физический HID-код; символ выбирает раскладка сима — см. №26)

По тому же DataChannel идут и `list`/`boot`/`attach`/`detach`: видео-трек молчит, пока не
выбран симулятор; `attach <udid>` спавнит сайдкар + ffmpeg и пишет H.264 в pre-negotiated трек,
`detach` (или новый `attach`) — останавливает и убивает сайдкар. Закрытие вкладки рвёт пир и
тушит сайдкар + ffmpeg.

> Клавиатура шлёт физические HID-коды клавиш — конкретный символ выбирает раскладка,
> активная в симуляторе. Если печатаешь латиницу, а в симе включён, например, русский ввод,
> символы будут «не те»: переключи раскладку в самом симуляторе (🌐 на экранной клавиатуре
> или `⌘Space`). См. `docs/decisions.md` №26.

> Для `jitterBufferTarget=0` (тюнинг буфера браузера) рекомендуется **Chrome**; Safari этот
> параметр игнорирует, но видео в нём работает.

> Почему screenshot→ffmpeg, а не сырой idb H.264 (`video_stream`): companion 1.1.8 отдаёт IDR
> с фиксированным GOP ~10с без возможности управления keyframe'ами через gRPC. На резкой смене
> сцены — артефакты до ~10с. Перейдя на свой ffmpeg-энкодер, мы владеем GOP и keyframe'ами.
> Подробно — `docs/decisions.md` №34–38.

### Удалёнка / рандеву (Phase 3b)

Демон дозванивается до signaling-брокера (исходящий WSS, ноль открытых портов на
Mac), регистрирует «комнату» по одноразовому `pairingToken` и печатает pairing-URL.
Браузер открывает URL (координаты — `signalingURL` + `token` + `daemonPubKey` — в
**фрагменте** `#…`, не в query), входит в комнату, обменивается offer/answer через
брокер и **проверяет подпись answer'а** по `daemonPubKey` (анти-MITM, Ed25519). Видео
и control идут P2P (DTLS-SRTP E2E); через брокер течёт только рукопожатие.

Reference-брокер — `cmd/simcast-signal` (в этом репо). STUN раздаётся всем; TURN —
только «подписчикам» (в этой фазе — стаб `--grant-turn`), по короткоживущим HMAC-кредам
для готового `coturn` (свой TURN не пишем). Деплой — `deploy/README.md`.

Локально (один хост, host-кандидаты):

```bash
# терминал 1 — брокер
go run ./cmd/simcast-signal --addr :9000 --stun stun:stun.l.google.com:19302
# терминал 2 — демон в remote-режиме
go run ./cmd/simcastd serve --addr :8080 --web ./web/debug \
  --signal ws://localhost:9000/ws --client-url http://localhost:8080/
# открыть напечатанный pairing-URL в браузере
```

Сигналинг — **только рукопожатие**: один offer/answer, затем сокет закрывается;
renegotiation нет (видео-трек pre-negotiated, решение #50). Реальный NAT/relay
локально не проверить — см. `deploy/README.md` и «deploy-only» сценарии.

## Remote pairing (Phase 3C)

Run the broker and the daemon, then pair a browser once and reconnect without QR:

```bash
# 1. Broker + subscription store (app secret must match the bench's dev value)
SIMCAST_APP_SECRET=dev-app-secret go run ./cmd/simcast-signal \
  --addr :9000 --db /tmp/simcast.db \
  --turn turn:relay.example:3478 --turn-secret secret   # TURN optional

# 2. Daemon: persistent identity + serve, debug client at :8080
go run ./cmd/simcastd serve --web web/debug --addr :8080 --signal ws://localhost:9000/ws
```

Press **P** in the daemon terminal to open a one-time pairing window; open the
printed URL and click **Pair this Mac**. The browser saves the Mac and reconnects
automatically afterwards (no QR). Revoke a device with
`simcastd unpair <clientPubKey>`. Inspect subscriptions by opening `/tmp/simcast.db`
in `sqlite3` / DB Browser.

Identity files live in `~/.simcast/` (`identity.key`, `clients.json`, both 0600).

## С чего начать (Bootstrap)

Открой папку агентом и попроси выполнить **Phase 0 (Bootstrap)** — см. `CLAUDE.md` и `docs/ROADMAP.md`.
Конкретный стартовый промпт — в `CLAUDE.md`, раздел «Стартовый промпт».
