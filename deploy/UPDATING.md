# Обновление серверной инфраструктуры simbeam

Операционная памятка: как сервер сам обновляет `simbeam-signal`, что и когда
ожидать, как выпустить новую версию, как наблюдать и чинить. Личные значения
(домен, IP, секреты) тут НЕ указаны — они живут только на сервере в
`/etc/simbeam/signal.env` (решение #68).

## TL;DR

- Чтобы обновить брокер на сервере — **просто выпусти git-тег `vX.Y.Z`**. Больше
  ничего на сервере руками делать не нужно.
- В течение ~10 минут systemd-таймер на VPS сам скачает новый бинарь, проверит
  checksum, атомарно подменит и рестартанёт сервис.
- **Автообновляется только `simbeam-signal`.** Caddy, coturn, env-файл и
  macOS-демон (`simbeamd`) — НЕ автообновляются (см. ниже).

## Что из чего состоит

| Компонент | Где | Обновляется |
|-----------|-----|-------------|
| `simbeam-signal` (брокер) | `/usr/local/bin/simbeam-signal`, юнит `simbeam-signal.service` | **авто** (pull из GitHub Releases) |
| Апдейтер | `/usr/local/bin/simbeam-signal-update.sh` | руками (`bootstrap.sh` / `git pull` + reinstall) |
| Таймер | `simbeam-signal-update.timer` (+ `.service`) | руками |
| coturn | пакет ОС + `/etc/turnserver.conf` | руками (`apt`) |
| Caddy | пакет ОС + `/etc/caddy/Caddyfile` | руками (`apt`) |
| Секреты/флаги | `/etc/simbeam/signal.env` | руками |
| `simbeamd` (macOS) | Homebrew cask | руками: `brew upgrade` |

## Как работает автообновление (pull-модель)

Модель «pull»: на сервере **нет** доступа к CI, а в CI **нет** доступа к серверу.
Сервер сам периодически опрашивает публичные GitHub Releases. Ноль серверных
секретов в репо и Actions (там только `HOMEBREW_TAP_TOKEN` для пуша cask в тап).

Цикл (`deploy/simbeam-signal-update.sh`, запускается таймером):

1. Спросить `GET /repos/<repo>/releases/latest` → последний тег (например `v0.1.1`).
2. Сравнить с текущей версией: `simbeam-signal --version` (печатает голую `X.Y.Z`)
   против `want="${tag#v}"` (тег без ведущего `v`).
3. Если совпадает → лог `up to date (X.Y.Z)`, выход 0. Если нет → обновляемся.
4. Скачать `simbeam-signal_<version>_linux_amd64.tar.gz` и `checksums.txt` из релиза.
5. **Проверить SHA-256** архива по `checksums.txt` (`sha256sum -c`). Не сошлось — скрипт
   падает (`set -euo pipefail`), бинарь НЕ трогается.
6. Распаковать, поставить в `<bin>.new`, **атомарно** `mv -f` поверх рабочего бинаря
   (та же ФС → атомарно), `systemctl restart simbeam-signal`.

Контракт имён, на котором всё держится (не ломать):
- архив: `simbeam-signal_<version>_linux_amd64.tar.gz` — одинаково в
  `.goreleaser.yaml` и в апдейтере;
- `--version` печатает голую `X.Y.Z` (без `v`), совпадает с GoReleaser `.Version`
  и с `want="${tag#v}"` в апдейтере.

## Расписание (чего ожидать по времени)

Из `simbeam-signal-update.timer`:
- `OnBootSec=2min` — первая проверка через 2 минуты после загрузки сервера;
- `OnUnitActiveSec=10min` — далее каждые ~10 минут;
- `Persistent=true` — если сервер был выключен в момент тика, проверка догоняется
  при старте.

Итог: после пуша тега новая версия приезжает на сервер **в пределах ~10 минут**
(плюс пара минут на сборку релиза в Actions). Перезапуск брокера — мгновенный;
короткий разрыв WS переживается клиентом (есть авто-reconnect).

## Как выпустить новую версию сервера

```bash
# на машине разработки, в чистом main:
git tag v0.1.1
git push origin v0.1.1
```

Дальше всё само:
1. GitHub Actions `release.yml` запускает GoReleaser → собирает оба бинаря, создаёт
   GitHub Release с архивами + `checksums.txt`, пушит обновлённый cask в тап.
2. Таймер на сервере в ближайший тик видит новый тег и обновляет брокер.

> Только git-тег. Никаких SSH на сервер, никаких ручных `scp`/`systemctl`.

## Как наблюдать

На сервере:
```bash
# когда следующий тик и когда был прошлый:
systemctl list-timers simbeam-signal-update.timer

# что делал апдейтер (скачал? «up to date»? ошибка checksum?):
journalctl -u simbeam-signal-update.service --no-pager | tail -30

# какая версия сейчас крутится:
simbeam-signal --version
systemctl status simbeam-signal --no-pager

# ручная проверка без установки (покажет «update available: X -> Y» или «up to date»):
/usr/local/bin/simbeam-signal-update.sh --dry-run

# принудительно обновить прямо сейчас, не дожидаясь таймера:
sudo systemctl start simbeam-signal-update.service
# (или: sudo /usr/local/bin/simbeam-signal-update.sh)
```

## Что ожидать в логах

- `simbeam-update: up to date (0.1.1)` — версии совпали, ничего не делалось.
- `simbeam-update: update available: 0.1.0 -> 0.1.1` затем
  `simbeam-update: updated to 0.1.1 and restarted simbeam-signal` — успешное обновление.
- `--dry-run` после «available» печатает `dry-run: not installing` и выходит.

## Режимы отказа и что происходит

| Сбой | Что делает скрипт | Последствие |
|------|-------------------|-------------|
| GitHub API недоступен / нет релизов | падает на резолве тега | старый брокер продолжает работать, повтор на следующем тике |
| Не сошёлся SHA-256 | `sha256sum -c` → `set -e` → выход | бинарь НЕ подменён, старая версия живёт |
| 404 на архив (имя не совпало) | `curl -f` → выход | то же; чини контракт имён |
| `systemctl restart` упал | ненулевой выход юнита | бинарь уже новый, но сервис не поднялся — смотри `status`/`journalctl -u simbeam-signal` |

Ключевое: подмена бинаря происходит **после** успешной проверки checksum и
**атомарно**, так что «полуобновлённого» состояния не бывает — либо старый бинарь,
либо целиком новый.

## Откат

Авто-отката нет. Если новая версия плохая:
1. Быстро: вручную поставить архив прошлой версии:
   ```bash
   ver=0.1.0
   cd /tmp && curl -fsSLO "https://github.com/<repo>/releases/download/v${ver}/simbeam-signal_${ver}_linux_amd64.tar.gz"
   tar -xzf simbeam-signal_${ver}_linux_amd64.tar.gz
   sudo install -m0755 simbeam-signal /usr/local/bin/simbeam-signal
   sudo systemctl restart simbeam-signal
   ```
   ⚠️ Таймер при следующем тике снова подтянет `latest` и «обновит» обратно. Чтобы
   удержать откат — временно останови таймер: `sudo systemctl stop simbeam-signal-update.timer`.
2. Правильно: выпустить новый исправленный тег `vX.Y.(Z+1)` — это «откат вперёд»,
   и таймер сам подтянет его.

## Что НЕ автообновляется (и как обновлять руками)

- **Сам апдейтер / юниты / Caddyfile** — это deploy-скаффолдинг из репо. После
  `git pull` в чекауте перезапусти `sudo ./deploy/bootstrap.sh` (идемпотентен) или
  переустанови нужный файл вручную и `systemctl daemon-reload`.
- **coturn / Caddy** — обычными `apt upgrade`. Конфиги (`/etc/turnserver.conf`,
  `/etc/caddy/Caddyfile`) правятся руками; их значения личные и в репо не лежат.
- **`/etc/simbeam/signal.env`** — секреты и флаги брокера; меняешь руками, затем
  `sudo systemctl restart simbeam-signal`. Шаблон — `deploy/signal.env.example`.
- **macOS-демон `simbeamd`** — ставится из Homebrew-тапа, обновляется `brew upgrade`
  (Mac сознательно не автообновляется, решение #67).
