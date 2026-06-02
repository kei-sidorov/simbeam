# Phase 2 — WebRTC (низкая задержка): дизайн

**Дата:** 2026-06-02
**Статус:** утверждён к реализации
**Предшествующее:** Phase 1 (JPEG/screenshot MVP) — `2026-06-02-phase1-jpeg-mvp-design.md`

## Цель

Низколатентный H.264-поток с симулятора на клиента по WebRTC, тачи по WebRTC
DataChannel. H.264 идёт из `idb_companion` `video_stream` в pion-трек **без
перекодирования** (`TrackLocalStaticSample`). Клиент Phase 2 — браузерный
debug-клиент (нативный iPad остаётся Phase 3).

**Definition of Done:** в браузере (режим RTC) видно живой низколатентный
H.264-поток по WebRTC; тачи/свайпы/Home/клавиатура проходят по DataChannel;
старый JPG-режим (Phase 1) продолжает работать как fallback.

## Принятые решения этой фазы

- **Клиент Phase 2 — браузерный debug-клиент.** Нативный iPad-клиент остаётся
  Phase 3. Браузер нативно принимает H.264 по WebRTC и рендерит в `<video>`.
- **Оба пути сосуществуют.** WebRTC — основной, JPEG/WS (`/session`) остаётся как
  fallback и debug. Phase 1 не трогаем.
- **Спайк первым шагом (шаг 0).** Главный неизвестный риск — скормить H.264 из
  `video_stream` в pion. История MJPEG (решение №22: на companion 1.1.8 MJPEG
  отдаёт один кадр и замирает) обязывает доказать пайплайн до постройки обвязки.
- **Подход A:** отдельный пакет `internal/rtc` (вся pion-логика) + отдельный
  WS-эндпоинт `/rtc`. `server.go` регистрирует роут одной строкой, роутинг в
  pion-логику не протекает. Команды ввода НЕ дублируются — переиспользуем
  `parseControl` / `keymap` / `ScaleTap` / `hid`.
- **Тачи — по DataChannel** (unreliable/unordered). Инкрементально дёшево (peer
  уже поднят ради видео), по роадмапу, один P2P-линк, готовность к Phase 4 (TURN).
- **Сигналинг — один WS-заход offer→answer, non-trickle.** Браузер — инициатор
  (recvonly видео + создаёт DataChannel), демон отвечает. ICE-кандидаты внутри SDP
  (на localhost сбор мгновенный). Только host-кандидаты, без STUN/TURN (решение №7).

## Архитектура

```
Браузер (web/debug)
  RTCPeerConnection ──video◄── H.264 трек ──┐
  DataChannel ───────тачи──►────────────────┤
                                            ▼
Mac: simcastd (Go)
  ├─ NEW  internal/rtc/        — pion peer, H.264-трек, обмен SDP, DataChannel→control
  │        ├─ peer.go          — PeerConnection, TrackLocalStaticSample, pump кадров
  │        └─ signal.go        — WS /rtc?udid=X: offer→answer (non-trickle)
  ├─ NEW  internal/idb: VideoStream(ctx, fps) (<-chan Frame, error) — video_stream H264 → access units
  ├─ REUSE internal/idb: Spawn (сайдкар), Describe, Tap/Swipe/Home/KeyPress (hid)
  ├─ REUSE internal/server: parseControl, keymap, ScaleTap — те же JSON-команды по DataChannel
  └─ REUSE /session (JPEG/WS) — без изменений, fallback
              │
              ▼
        idb_companion (sidecar) → iOS Simulator
```

**Границы компонентов:**
- `internal/rtc` — единственное место, где живёт pion. Знает про SDP, треки,
  DataChannel. Не знает про HTTP-роутинг, не парсит команды сам (зовёт `parseControl`).
- `internal/idb` прирастает одним методом `VideoStream` — симметрично
  `ScreenshotStream`. H.264-нарезка изолирована здесь, про WebRTC метод не знает.
- Управление переиспользует `parseControl` + `ScaleTap` + `hid` без дублирования —
  DataChannel подаёт туда те же байты, что сейчас подаёт WS `/session`.

**Новая зависимость:** `github.com/pion/webrtc/v4` (+ H.264-хелперы pion). Остальное
уже в go.mod.

## Шаг 0 — спайк (снятие риска)

Минимальный вертикальный срез, доказывающий, что H.264 от idb непрерывно течёт и
доходит до браузера движущейся картинкой. Сигналинг — самый дубовый (HTTP POST
offer→answer или захардкоженный обмен); он тут НЕ предмет проверки.

**Состав:**
- Черновой `idb.VideoStream`: открыть `video_stream` с `Format=H264`, читать payload,
  логировать размер/частоту чанков (подтвердить непрерывность, а не «один кадр и замер»).
- Прогон байтов через H.264-парсер (pion `h264reader` / NAL-сплит) → access unit'ы.
- Минимальный pion: один `TrackLocalStaticSample`, `WriteSample` каждым кадром.
- Страница-заглушка с одним `<video>`.

**Критерий прохождения:** в браузере видно непрерывное движущееся видео симулятора
(меняешь экран — картинка меняется в реальном времени). Латентность НЕ замеряем.

**Если не проходит** (H.264 кривой/прерывистый): фиксируем находку в `decisions.md`,
Phase 2 разворачивается (транскодинг или video-to-file) — отдельный разговор, только
если упрёмся.

**Судьба кода:** ядро ингеста (`VideoStream`) дочищается до продакшна; дубовый
сигналинг выбрасывается; страница-заглушка вливается в `web/debug`.

## Компонент: H.264-ингест (`internal/idb.VideoStream`)

Продакшн-версия ядра из спайка, рядом с `ScreenshotStream`.

```go
type Frame struct {
    Data     []byte        // один access unit (H.264 NAL'ы, Annex-B)
    Duration time.Duration // для media.Sample; из fps
}
func (c *Client) VideoStream(ctx context.Context, fps uint64) (<-chan Frame, error)
```

- Открывает `video_stream` gRPC-стрим с `Start{Format: H264, Fps: fps, ScaleFactor: 1.0}`.
  `fps` старт = **30**; `CompressionQuality`/`ScaleFactor` — дефолты (решение №23,
  качество пока не крутим).
- Читает `VideoStreamResponse`, склеивает payload, режет на NAL'ы, собирает в
  access unit'ы.
- SPS/PPS: отдаём access unit'ы как есть — pion `TrackLocalStaticSample` обрабатывает
  параметрические NAL'ы при упаковке в RTP. (Подтверждается спайком.)
- `Duration` из `fps` (1/30 c). Если idb проставляет надёжные PTS — используем их,
  иначе равномерный шаг. (Подтверждается спайком.)
- На `ctx.Done()` — закрыть gRPC-стрим и канал. Транзиентные ошибки — лог + продолжаем
  (как `ScreenshotStream`); фатальная — закрыть канал, сессия рвётся.

**Решается на спайке (не в дизайне):** точный fps, нужно ли руками разделять SPS/PPS,
есть ли у idb надёжные PTS, конкретный H.264-профиль.

## Компонент: pion-peer (`internal/rtc/peer.go`)

Жизненный цикл одной WebRTC-сессии:
- `PeerConnection` (pion), без STUN/TURN — только host-кандидаты.
- Один видеотрек `TrackLocalStaticSample`, кодек H.264. В `MediaEngine` объявляем
  H.264-payload совместимо с браузерным `<video>` (baseline/constrained;
  profile-level-id уточняется спайком).
- `idb.Spawn` (сайдкар) → `Describe` (для `ScaleTap`) → `VideoStream`; горутина
  качает кадры: `track.WriteSample(media.Sample{Data, Duration})`.
- `OnDataChannel("control")`: входящие сообщения → `parseControl` → `ScaleTap`/`hid`
  (тот же код, что в `/session`).
- Teardown: на закрытие peer / `ctx.Done()` — стоп `VideoStream`, kill сайдкара.
  Симметрично нынешнему `handleSession`.

## Компонент: сигналинг (`internal/rtc/signal.go`, WS `/rtc?udid=X`)

```
браузер                          simcastd /rtc
  createOffer + setLocal  ──────►
  (ждёт сбора ICE)
  send offer SDP          ──────►  setRemote(offer)
                                   spawn sidecar, video track, datachannel
                                   createAnswer + setLocal
                                   (ждёт сбора ICE — на localhost мгновенно)
                          ◄──────  send answer SDP
  setRemote(answer)
  ── видео и DataChannel поднимаются по P2P ──
```

Один заход offer→answer, ICE внутри SDP (non-trickle). После обмена WS простаивает/
закрывается — медиа и control идут по P2P. `server.go` регистрирует `/rtc` одной
строкой `mux.HandleFunc`, как `/session`.

## Компонент: браузерный клиент (`web/debug/index.html`)

Два режима с переключателем (две кнопки **RTC** / **JPG**):
- Дефолт — **RTC** (витрина фазы); **JPG** — fallback в один клик.
- Переключение рвёт активное соединение текущего режима и поднимает другое. Выбор
  симулятора / boot — общий для обоих.
- **RTC:** `RTCPeerConnection`, `<video autoplay playsinline>`,
  `createDataChannel("control")`, обмен SDP по WS `/rtc`. Тачи/свайпы/Home/клавиатура
  шлются **тем же JSON** (`{"type":"tap",...}` и т.д.), что в JPG-режиме, но через
  `dataChannel.send()`. JS-обработчики ввода переиспользуются — меняется только труба.
- **JPG:** без изменений (Phase 1).
- Координаты — нормализованный `[0,1]` (решения №20/24); `<video>` отдаёт размеры
  как `<img>`.

## Обработка ошибок / teardown

- ICE завис / peer `failed`|`disconnected` → клиент показывает ошибку, можно
  переключиться на JPG. Сервер по закрытию peer убивает сайдкар.
- Сайдкар не стартовал / `Describe` упал → ошибка по WS `/rtc` до обмена answer
  (как сейчас в `/session`).
- `VideoStream` оборвался → закрываем peer, сессия рвётся (симметрично Phase 1
  «stream ended → teardown»).
- Один сайдкар на сессию. Параллельные RTC + JPG к одному udid в MVP не
  поддерживаем (последний выигрывает) — упрощение, не предмет фазы.

## Тестирование

- **Юнит:** H.264-сплиттер на access unit'ы (фиксированный байтовый вектор →
  ожидаемые границы NAL) в `internal/idb`. Путь DataChannel→control покрыт
  существующими тестами `parseControl`/`keymap`/`ScaleTap`.
- **`internal/rtc`:** тест сборки SDP-ответа / регистрации трека настолько, насколько
  pion даёт юнит-тестить без реального peer (минимально).
- **Спайк-критерий** (шаг 0) — отдельная веха: видно непрерывное движущееся видео.
- **Ручной E2E (DoD):** браузер, RTC-режим — низколатентное живое видео + тап/свайп/
  Home/клавиши по DataChannel; JPG-режим работает как раньше.

## Порядок реализации

1. **Шаг 0 — спайк.** Снять риск H.264. Пока не пройдёт — дальше не идём.
2. H.264-ингест (`internal/idb.VideoStream`) — дочистка ядра из спайка.
3. `internal/rtc` — pion-peer + `/rtc` сигналинг (дубовый сигналинг из спайка
   выбрасывается).
4. DataChannel → control (переиспользуем `parseControl`/`ScaleTap`/`hid`).
5. Браузер: переключатель RTC/JPG, `<video>`, DataChannel.

## Сознательно вне скоупа Phase 2

- Нативный iPad-клиент (Phase 3).
- STUN/TURN, NAT-traversal, защищённый сигналинг (Phase 4).
- Адаптивный битрейт, своя видео-пайплайн (отложено в ARCHITECTURE.md).
- Параллельные RTC+JPG к одному udid.
- Trickle ICE, реконнект (понадобятся позже; non-trickle достаточно для localhost).
