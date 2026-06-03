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
│   ├── ROADMAP.md             # фазы: 0 bootstrap → 1 JPEG MVP → 2 WebRTC → 3 клиент
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

## Запуск стрима (Phase 1 — JPEG MVP)

```bash
go run ./cmd/simcastd serve --web ./web/debug
# или: make run
```

Открой `http://localhost:8080/`, выбери симулятор (кнопка забутит его, если он не запущен),
смотри живые кадры экрана по WebSocket. Управление: **клик** = тап, **drag** = свайп,
кнопка **Home** = Home, **физическая клавиатура** печатает в устройство (пока открыта вкладка).

> Клавиатура шлёт физические HID-коды клавиш — конкретный символ выбирает раскладка,
> активная в симуляторе. Если печатаешь латиницу, а в симе включён, например, русский ввод,
> символы будут «не те»: переключи раскладку в самом симуляторе (🌐 на экранной клавиатуре
> или `⌘Space`). См. `docs/decisions.md` №26.

Как это устроено: на WS-коннект `/session?udid=X` демон спавнит сайдкар
`idb_companion --udid X --grpc-port N`, по gRPC берёт размеры экрана (`describe`), опрашивает
`screenshot` ~10 раз/сек и отдаёт PNG-кадры в WS, а тапы из браузера шлёт через `hid`.
Сайдкар убивается при закрытии вкладки.

> Почему скриншоты, а не `video_stream`: на companion 1.1.8 MJPEG-стрим отдаёт лишь один кадр
> на статичном экране, а единственный непрерывный формат H.264 не рисуется напрямую в `<img>`.
> Низколатентное H.264-видео — это Phase 2 (WebRTC). См. `docs/decisions.md` №22.

Без `--web` демон отдаёт только API — для подключения собственных клиентов:

| Endpoint | Назначение |
|----------|-----------|
| `GET /api/simulators` | список симуляторов (JSON) |
| `POST /api/boot` `{"udid":"…"}` | забутить симулятор |
| `WS /session?udid=X` | вниз — бинарные PNG-кадры (от `idb screenshot`); вверх — JSON-команды (см. ниже) |
| `WS /rtc?udid=X` | сигналинг WebRTC (offer→answer, non-trickle); видео H.264 и control-тачи идут по P2P (медиа-трек + DataChannel) |

Команды вверх по `/session` (координаты нормализованы [0,1] от показанного кадра):

- `{"type":"tap","x":..,"y":..}`
- `{"type":"swipe","x1":..,"y1":..,"x2":..,"y2":..,"duration":<сек>}`
- `{"type":"home"}`
- `{"type":"key","key":"<KeyboardEvent.key>"}` (физический HID-код; символ выбирает раскладка сима — см. №26)

## Запуск стрима (Phase 2 — WebRTC)

Та же команда запускает оба режима:

```bash
go run ./cmd/simcastd serve --web ./web/debug
# или: make run
```

Открой `http://localhost:8080/`, выбери симулятор. В правом верхнем углу дебаг-клиента есть
переключатель **RTC / JPG**; по умолчанию активен **RTC**.

**RTC-режим** — низколатентный H.264 по WebRTC. Пайплайн:

```
idb screenshot (PNG-поллинг) → ffmpeg (h264_videotoolbox, короткий GOP ~1–2с,
  половинное разрешение — браузер апскейлит) → pion → <video>
```

Тачи, свайпы, кнопка Home и физическая клавиатура идут по **WebRTC DataChannel** — тот же
JSON-формат, что в JPG-режиме (`tap`, `swipe`, `home`, `key`).

Сигналинг: один WS-заход `/rtc?udid=X` — браузер шлёт offer, сервер отвечает answer (non-trickle,
только host-кандидаты, без STUN/TURN — скоуп localhost). Закрытие вкладки или смена симулятора
рвёт сессию и убивает сайдкар + ffmpeg-процесс.

> Для `jitterBufferTarget=0` (тюнинг буфера браузера) рекомендуется **Chrome**; Safari этот
> параметр игнорирует, но RTC-режим в нём работает.

**JPG-режим** — тот же Phase 1 fallback: скриншот-поллинг по WS `/session`, кадры рисуются
в `<img>`. Подходит для отладки, если H.264 даёт артефакты.

> Почему screenshot→ffmpeg, а не сырой idb H.264 (`video_stream`): companion 1.1.8 отдаёт IDR
> с фиксированным GOP ~10с без возможности управления keyframe'ами через gRPC. На резкой смене
> сцены — артефакты до ~10с. Перейдя на свой ffmpeg-энкодер, мы владеем GOP и keyframe'ами.
> Подробно — `docs/decisions.md` №34–38.

## С чего начать (Bootstrap)

Открой папку агентом и попроси выполнить **Phase 0 (Bootstrap)** — см. `CLAUDE.md` и `docs/ROADMAP.md`.
Конкретный стартовый промпт — в `CLAUDE.md`, раздел «Стартовый промпт».
