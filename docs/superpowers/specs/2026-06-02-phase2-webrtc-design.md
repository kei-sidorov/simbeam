# Phase 2 — WebRTC (низкая задержка): дизайн

**Дата:** 2026-06-02
**Статус:** утверждён к реализации (пересмотрен после спайка)
**Предшествующее:** Phase 1 (JPEG/screenshot MVP) — `2026-06-02-phase1-jpeg-mvp-design.md`

> **Пересмотр после спайка (2026-06-02).** Исходный замысел — отдавать сырой H.264 из
> idb `video_stream` в pion без перекодирования. Спайк это опроверг: idb шлёт IDR раз в
> ~10с и не вставляет keyframe на смену сцены (в gRPC нет управления GOP). Артефакты до
> ~10с на резких сменах. Решение пересмотрено (decisions №34–38): **захватываем кадры
> через `screenshot` (путь Phase 1) и кодируем H.264 сами через `ffmpeg`
> (`h264_videotoolbox`, короткий GOP) — мы владеем энкодером и keyframe'ами.** Спайк
> подтвердил: идеальное качество, артефактов нет, ~0.7с задержки @15fps localhost.

## Цель

Низколатентный H.264-поток с симулятора на клиента по WebRTC, тачи по WebRTC
DataChannel. Кадры захватываются через idb `screenshot` и кодируются в H.264 нашим
`ffmpeg`-подпроцессом (аппаратный `h264_videotoolbox`, короткий GOP — keyframe раз в
~1–2с), затем без дальнейшего перекодирования пишутся в pion `TrackLocalStaticSample`.
Клиент Phase 2 — браузерный debug-клиент (нативный iPad остаётся Phase 3).

**Definition of Done:** в браузере (режим RTC) видно живой H.264-поток по WebRTC с
ощутимо низкой задержкой (суб-секунда) и без артефактов на сменах сцены; тачи/свайпы/
Home/клавиатура проходят по DataChannel; старый JPG-режим (Phase 1) работает как fallback.

## Принятые решения этой фазы

- **Клиент Phase 2 — браузерный debug-клиент.** Нативный iPad — Phase 3. Браузер нативно
  принимает H.264 по WebRTC и рендерит в `<video>`.
- **Оба пути сосуществуют.** WebRTC — основной, JPEG/WS (`/session`) — fallback и debug.
  Phase 1 не трогаем. Оба пути используют один и тот же источник — idb `screenshot`.
- **Видеопайплайн (decisions №34–37):** idb `screenshot` (PNG-поллинг) → `ffmpeg`
  (`h264_videotoolbox`, короткий GOP, low-latency флаги) → H.264 Annex-B → парсинг в
  access unit'ы → pion `TrackLocalStaticSample`. Мы владеем энкодером, поэтому keyframe'ы
  под нашим контролем — это и устраняет проблему idb-GOP.
- **`ffmpeg` — обязательная внешняя зависимость** (подпроцесс, как idb; brew
  `depends_on ffmpeg`). idb по-прежнему захватывает экран — мы лишь перекодируем его
  вывод; это НЕ «свой capture-пайплайн» (тот остаётся отложен).
- **Подход A:** пион-логика в `internal/rtc`; парсинг H.264 + энкодер-подпроцесс в
  `internal/encoder`; WS-эндпоинт `/rtc` и оркестрация — в пакете `server`, который
  переиспользует `parseControl`/`ScaleTap`/`hid` (ввод не дублируется).
- **Тачи — по WebRTC DataChannel** (unreliable/unordered).
- **Сигналинг — один WS-заход offer→answer, non-trickle, браузер-инициатор;** только
  host-кандидаты, без STUN/TURN.

## Архитектура

```
Браузер (web/debug)
  RTCPeerConnection ──video◄── H.264 трек ──┐   (receiver.jitterBufferTarget=0)
  DataChannel ───────тачи──►────────────────┤
                                            ▼
Mac: simbeamd (Go)
  ├─ REUSE internal/idb: Spawn (сайдкар), Describe, ScreenshotStream (PNG-кадры), hid
  ├─ NEW  internal/encoder/    — PNG-кадры → H.264 access unit'ы
  │        ├─ ffmpeg.go         — спавн ffmpeg(h264_videotoolbox), stdin PNG / stdout H.264
  │        └─ nal.go            — Annex-B NAL-сплиттер + сборка access unit'ов (из спайка)
  ├─ NEW  internal/rtc/        — pion peer, H.264-трек, обмен SDP, DataChannel→control
  │        ├─ peer.go           — PeerConnection, TrackLocalStaticSample, Answer, WriteFrame
  │        └─ (signaling — в server)
  ├─ REUSE internal/server: parseControl, keymap, ScaleTap, applyControl(нов.)
  ├─ NEW  internal/server/rtc.go — WS /rtc: sidecar→ScreenshotStream→encoder→pion + DataChannel→control
  └─ REUSE /session (JPEG/WS) — без изменений, fallback (тоже на screenshot)
              │
              ▼
        idb_companion (sidecar) ──screenshot──► iOS Simulator
              ⇣ (PNG)            └─ hid ◄── тачи
          ffmpeg (h264_videotoolbox)
```

**Границы компонентов:**
- `internal/encoder` — единственное место, где живёт H.264 (ffmpeg-подпроцесс +
  Annex-B-парсинг). Вход — канал PNG-кадров (`<-chan []byte`), выход — канал
  `Frame{Data, Duration}`. Не знает про idb, pion, HTTP.
- `internal/rtc` — единственное место, где живёт pion. Speaks raw SDP, принимает
  `onControl([]byte)`. Не знает про idb/encoder/HTTP/команды.
- `internal/idb` — без изменений для Phase 2: переиспользуем `ScreenshotStream`, `Spawn`,
  `Describe`, `hid`. `VideoStream` НЕ добавляем (от idb-H.264 отказались).
- Пакет `server` оркеструет: spawn → ScreenshotStream → encoder → rtc.Session, и
  DataChannel-сообщения гонит через `parseControl`+`applyControl` (тот же код, что `/session`).

**Зависимости:** `github.com/pion/webrtc/v4` (в go.mod), внешний бинарь `ffmpeg` с
энкодером `h264_videotoolbox` (проверяется в рантайме).

## Компонент: энкодер (`internal/encoder`)

```go
type Frame struct {
    Data     []byte        // один H.264 access unit (NAL'ы в Annex-B, со start-кодами)
    Duration time.Duration // 1/fps, для media.Sample
}

// Encode спавнит ffmpeg(h264_videotoolbox), пишет PNG-кадры из png в его stdin,
// читает H.264 из stdout, режет на NAL'ы, собирает access unit'ы и эмитит Frame
// до закрытия ctx (тогда канал закрывается, ffmpeg убивается).
func Encode(ctx context.Context, png <-chan []byte, fps int) (<-chan Frame, error)

// Available проверяет, что ffmpeg и энкодер h264_videotoolbox доступны.
func Available() error
```

**ffmpeg argv (подтверждено спайком, low-latency):**
```
ffmpeg -hide_banner -loglevel warning \
  -fflags nobuffer -flags low_delay -analyzeduration 0 \
  -f image2pipe -vcodec png -framerate <fps> -i pipe:0 \
  -an -c:v h264_videotoolbox -realtime 1 -profile:v baseline -g <fps*2> -b:v 8M -pix_fmt yuv420p \
  -flush_packets 1 -max_delay 0 -f h264 pipe:1
```
- `-analyzeduration 0` критичен: иначе демуксер `image2pipe` набирает ~3с стартового
  бэклога, который держится константно (decision №37).
- `baseline` + `-f h264` → Annex-B без B-кадров (то, что ждёт H.264-пакетайзер pion).
- `-g <fps*2>` ≈ keyframe раз в ~2с (тюнится).
- idb `screenshot` отдаёт **PNG** (у `ScreenshotRequest` нет поля формата) → вход
  `-f image2pipe -vcodec png`.

**Парсинг H.264 (баги, найденные спайком — обязательны):**
- NAL-сплиттер пропускает **полную** длину start-code (3 или 4 байта), иначе ловит
  «вложенный» 3-байтный код в 4-байтном и плодит мусорные NAL'ы (decision №38).
- Граница access unit — **перед** ведущими SEI/SPS/PPS (типы 6/7/8) или AUD(9)/VCL(1–5),
  чтобы SPS+PPS+IDR ехали в одном сэмпле. Иначе браузер не декодирует keyframe (decision №38).

## Компонент: pion-сессия (`internal/rtc/peer.go`)

- `PeerConnection` (pion), без STUN/TURN — только host-кандидаты.
- Один видеотрек `TrackLocalStaticSample`, кодек H.264 (дефолтный MediaEngine,
  baseline-совместимо).
- `OnDataChannel("control")`: входящие сообщения → callback `onControl([]byte)`.
- `Answer(offerSDP) (answerSDP, error)`: non-trickle (ждёт `GatheringCompletePromise`).
- `WriteFrame(data, dur)`: пишет access unit в трек.
- `OnClose(fn)` / `Close()`: teardown при failed/disconnected/closed.

## Компонент: сигналинг + оркестрация (`internal/server/rtc.go`, WS `/rtc?udid=X`)

```
браузер                          simbeamd /rtc
  createOffer + setLocal  ──────►
  (ICE gathering complete)
  send offer SDP          ──────►  setRemote(offer)
                                   spawn sidecar; Describe; ScreenshotStream → encoder.Encode
                                   rtc.New(onControl) → Answer(offer)
                          ◄──────  send answer SDP
  setRemote(answer)
  receiver.jitterBufferTarget=0
  ── видео (encoder→WriteFrame) и DataChannel(control→applyControl) по P2P ──
```
`server.go` регистрирует `/rtc` одной строкой. При старте/коннекте проверяется
`encoder.Available()`; если ffmpeg нет — внятная ошибка клиенту (предложить `brew install ffmpeg`).

## Компонент: браузерный клиент (`web/debug/index.html`)

Две кнопки **RTC** / **JPG** (дефолт RTC):
- **RTC:** `RTCPeerConnection`, `<video autoplay playsinline muted>`,
  `createDataChannel("control")`, обмен SDP по WS `/rtc`. После `ontrack` —
  `receiver.jitterBufferTarget=0` и `playoutDelayHint=0` (Chrome, в try/catch). Тачи/свайпы/
  Home/клавиатура — тем же JSON, что в JPG-режиме, но через `dataChannel.send()`.
- **JPG:** без изменений (Phase 1, PNG-кадры по WS в `<img>`).
- Координаты — нормализованный `[0,1]` (решения №20/24); `<video>` отдаёт размеры как `<img>`.

## Обработка ошибок / teardown

- `ffmpeg` недоступен → `encoder.Available()` ошибка по WS `/rtc` до answer (совет про brew).
- ICE завис / peer failed|disconnected → клиент показывает ошибку, можно переключиться на JPG;
  сервер по закрытию peer убивает сайдкар и ffmpeg.
- ScreenshotStream/encoder оборвались → закрываем peer, сессия рвётся.
- Сайдкар не стартовал / `Describe` упал → ошибка по WS `/rtc` до answer.
- Один сайдкар + один ffmpeg на сессию.

## Тестирование

- **Юнит:** Annex-B NAL-сплиттер (вкл. 4-байтный start-code) и сборка access unit'ов
  (вкл. группировку SPS/PPS/SEI с IDR) в `internal/encoder` — фиксированные байтовые
  векторы. Путь DataChannel→control покрыт существующими тестами `parseControl`/`keymap`/`ScaleTap`.
- **`internal/encoder`:** тест построения ffmpeg argv; `Available()` (skip, если ffmpeg нет).
- **`internal/rtc`:** тест сборки SDP-ответа (реальный offer от тестового pion-peer → `m=video`).
- **`/rtc`:** быстрый тест 400 при отсутствии `?udid=`.
- **Ручной E2E (DoD):** браузер, RTC — низколатентное (суб-секунда) живое видео без
  артефактов на смене сцены + тач/свайп/Home/клавиши по DataChannel; JPG-режим работает.

## Порядок реализации (см. план)

1. Annex-B NAL-сплиттер (`internal/encoder/nal.go`) — TDD, фиксы из спайка.
2. Сборка access unit'ов (`auAssembler` + `startsNewAU`) — TDD, фиксы из спайка.
3. ffmpeg-энкодер (`internal/encoder/ffmpeg.go` + `Available`) — обвязка подпроцесса.
4. Вынести `applyControl` (переиспользование ввода).
5. `internal/rtc.Session` (pion peer).
6. WS `/rtc` + оркестрация (screenshot→encoder→pion) в `server`.
7. Браузер: переключатель RTC/JPG, `<video>`, DataChannel, jitterBufferTarget=0.
8. Ручной E2E (DoD).
9. Тюнинг латентности (цель — суб-300мс: fps↑, `-vf scale`↓, пейсинг) — best-effort.
10. Уборка спайка + README (зависимость ffmpeg).

## Сознательно вне скоупа Phase 2

- Нативный iPad-клиент (Phase 3).
- STUN/TURN, NAT-traversal, защищённый сигналинг (Phase 4).
- VP9/AV1 screen-content кодеки (выгода на узком канале — Phase 4).
- Собственный capture-пайплайн (ScreenCaptureKit) — отложено (idb захватывает, мы только
  перекодируем его вывод).
- Адаптивный битрейт.
- Параллельные RTC+JPG к одному udid; trickle ICE; реконнект.
