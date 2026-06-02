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
| WebRTC (сервер) | **pion** | лучший серверный WebRTC; умеет отдавать готовый H.264-трек без энкодинга |
| Транспорт к companion | **gRPC** (`grpc-go`, стабы из `idb.proto`) | родной API idb; нужны буквально `describe` + `video_stream` + `hid` |
| Клиент (iPad) | **Swift + GoogleWebRTC** | приёмная сторона WebRTC на iOS — хорошо проторённый путь |
| Дистрибуция демона | **Homebrew tap + GoReleaser** | prebuilt бинари, без нотаризации (brew не вешает quarantine), `depends_on idb-companion` |

Подробное обоснование всех решений — в `docs/ARCHITECTURE.md`.
План по фазам — в `docs/ROADMAP.md`.

## Минимальная нужная RPC-поверхность

Из всего `idb.proto` нам нужно по сути 3 вызова:

- `describe` — размер экрана симулятора (для маппинга координат тача);
- `video_stream` (streaming) — H.264 наружу;
- `hid` (streaming) — тачи/свайпы/кнопки внутрь (tap = touch down + up, swipe = down + moves + up, Home = button event).

Lifecycle (список, бут) — это **не gRPC**, а CLI-флаги самого companion'а:
`idb_companion --list 1`, `idb_companion --boot <UDID>`, `idb_companion --udid <UDID> --grpc-port <N>`.

## Архитектура (одной схемой)

```
iPad (Swift + GoogleWebRTC)
   │  видео ◄── WebRTC ─────────────┐
   │  тачи  ──► WS / DataChannel ───┐│
   ▼                                ▼▼
Mac: simcastd (Go)
   ├─ spawn → idb_companion --udid <UDID> --grpc-port N   (сайдкар-процесс)
   ├─ gRPC  → describe / video_stream / hid
   └─ pion  → WebRTC-трек (H.264 как есть) + control-канал
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
- **Go** (для разработки демона).

## С чего начать (Bootstrap)

Открой папку агентом и попроси выполнить **Phase 0 (Bootstrap)** — см. `CLAUDE.md` и `docs/ROADMAP.md`.
Конкретный стартовый промпт — в `CLAUDE.md`, раздел «Стартовый промпт».
