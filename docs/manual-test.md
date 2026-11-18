# Ручная проверка Stealth Box

План для macOS и Linux-ВМ, команды соответствуют v0.4.3. Выполняй шаги по порядку и сравнивай результат с ожидаемым. При ошибке остановись на этом шаге и сохрани вывод.

Нужна действующая ВМ с SSH-алиасом `dev-vm`. Stealth Box подключается к существующей ВМ; создание облачных ресурсов в эту инструкцию не входит.

## Где выполняются команды

Держи два терминала:

| Терминал | Что в нём делать |
|---|---|
| **A — Mac** | Настраивать Stealth Box, импортировать исходники, управлять мостом |
| **B — ВМ** | Открыть через `stealthbox`; работать в tmux, запускать агентов и прогоны |

```text
Терминал на Mac → SSH → tmux на ВМ → Codex / Claude / обычная оболочка
                                      │
                                      ├─ runner vm    → команды и Docker на ВМ
                                      └─ runner local → копия исходников на Mac
                                                       → команды и Docker на Mac
```

Три каталога имеют разное назначение:

- **VMRoot** — `~/Projects` на ВМ: здесь агент читает и меняет код.
- **LocalRoot** — `~/Projects` на Mac: исходники для явного импорта/экспорта.
- **RunnerRoot** — `~/.local/share/stealthbox/runners` на Mac: отдельные копии для прогонов.

`local` в командах Stealth Box означает Mac. Обычная команда, введённая в оболочке ВМ, выполняется на ВМ.

## 1. Проверить доступ и локальный Docker

**Терминал A — Mac.** Запусти Docker Desktop, затем:

```bash
stealthbox --version
ssh dev-vm 'hostname; uname -s'
docker info
```

**Ожидается:** версия Stealth Box, имя ВМ, `Linux`, информация Docker без ошибки.

Если SSH не работает, исправь IP, пользователя и путь к ключу в блоке `Host dev-vm` файла `~/.ssh/config`. Проверь обычный SSH до следующих шагов. Для автоматической подготовки ВМ пользователю нужен `sudo` без запроса пароля либо уже установленные необходимые пакеты.

## 2. Подготовить Docker на ВМ

Если Docker на ВМ уже доступен обычному пользователю, переходи к шагу 3.

**Терминал A — Mac:**

```bash
ssh dev-vm
```

**В открывшейся оболочке ВМ, для Ubuntu:**

```bash
sudo apt-get update
sudo apt-get install -y docker.io
sudo usermod -aG docker "$(id -un)"
sudo systemctl enable --now docker
exit
```

**Снова на Mac:**

```bash
ssh dev-vm 'docker info'
```

**Ожидается:** Docker доступен без `sudo`. Новое SSH-подключение подхватывает членство в группе `docker`. Подготовь это до создания tmux-сессии.

## 3. Настроить один конфиг

**Терминал A — Mac.** Если конфиг ещё не создан:

```bash
stealthbox init --host dev-vm --runner local
```

Если конфиг уже есть, открой его:

```bash
stealthbox config --edit
```

В объекте `workspace` проверь следующие поля; остальные существующие поля сохраняй:

```json
{
  "host": "dev-vm",
  "vm_root": "~/Projects",
  "runner": "local",
  "prepare": true,
  "shell_integration": true
}
```

Если сменился SSH-пользователь ВМ, очисти `workspace.remote_dir` и `bridge.remote_socket`, поставив `""`: setup определит пути заново.

Задай каталоги и транспорт:

```bash
stealthbox config \
  --vm-root '~/Projects' \
  --local-root "$HOME/Projects" \
  --runner-root "$HOME/.local/share/stealthbox/runners" \
  --sync-transport rsync
```

**Ожидается:** один конфиг для ВМ и всего Projects. Регистрировать каждый репозиторий или выбирать агента в конфиге не нужно.

## 4. Перенести оформление и подготовить ВМ

**Из своего работающего локального tmux на Mac:**

```bash
stealthbox theme
```

**Затем в терминале A, вне локального tmux:**

```bash
stealthbox setup
stealthbox bridge-start
stealthbox bridge-status
```

**Ожидается:** успешная подготовка ВМ и работающая локальная служба моста. Первый setup может занять время: он устанавливает утилиты и агентов.

`bridge-status` проверяет службу на Mac. Готовность обратного SSH-туннеля проверяется с ВМ в шаге 6.

## 5. Создать и импортировать маленький проект

**Терминал A — Mac.** Используй новый тестовый каталог `stealthbox-demo`. Если такой каталог уже содержит твои данные, выбери другое имя и замени его в последующих командах.

```bash
mkdir -p ~/Projects/stealthbox-demo
cd ~/Projects/stealthbox-demo
git init
printf 'hello from Mac\n' > hello.txt

stealthbox source import \
  --path stealthbox-demo \
  --plan /tmp/stealthbox-demo-import.json

less /tmp/stealthbox-demo-import.json
```

Просмотри переносимые файлы. Из `less` выйди клавишей `q`, затем:

```bash
stealthbox source apply --plan /tmp/stealthbox-demo-import.json
```

**Ожидается:** исходники появились под VMRoot. Планирование и применение — два отдельных действия. При повторной проверке используй новое имя файла плана.

## 6. Открыть tmux на ВМ и проверить мост

**Терминал B — пока ещё Mac, вне локального tmux:**

```bash
stealthbox
```

**В открывшемся tmux на ВМ:**

```bash
hostname
uname -s
pwd
stealthbox bridge-health
```

**Ожидается:** имя ВМ, `Linux`, каталог Projects и успешная проверка моста.

Шрифт и палитра терминала остаются на Mac; сама tmux-сессия и её процессы работают на ВМ.

**Не переходи к локальным прогонам, пока `bridge-health` возвращает ошибку.** Штатное подключение ожидает готовность туннеля. Успешный `bridge-status` на Mac сам по себе не подтверждает эту связь.

## 7. Проверить проект и обнаружение репозиториев

**Терминал B — ВМ:**

```bash
cd ~/Projects/stealthbox-demo
test -e .git || git init
cat hello.txt
stealthbox list --discover
```

**Ожидается:** `hello from Mac`; в списке — `stealthbox-demo`.

Импорт исключает `.git`. `git init` задаёт границу тестового репозитория, но не восстанавливает историю. Для обычной разработки клонируй настоящий репозиторий на ВМ.

## 8. Проверить окна и сохранение сессии

Создай новое окно своими обычными клавишами tmux. При стандартном префиксе это **Ctrl+b, затем c**. Если импортирован свой префикс, используй его.

**В одном из окон ВМ:**

```bash
while true; do date; sleep 2; done
```

Отсоединись: **префикс tmux, затем d**.

**Снова на Mac:**

```bash
stealthbox
```

**Ожидается:** возвращение в прежние окна; вывод времени продолжается. Останови свою команду через `Ctrl+C`. Переключись между окнами и проверь их сохранность.

## 9. Сравнить Docker на ВМ и Mac

**Терминал B — ВМ:**

```bash
cd ~/Projects/stealthbox-demo
```

Docker на ВМ:

```bash
stealthbox run --runner vm -- sh -c \
  'pwd; uname -s; docker run --rm -v "$PWD:/work:ro" alpine:3.20 cat /work/hello.txt'
```

Docker на Mac:

```bash
stealthbox run --runner local -- sh -c \
  'pwd; uname -s; docker run --rm -v "$PWD:/work:ro" alpine:3.20 cat /work/hello.txt'
```

**Ожидается:**

| Прогон | Каталог и система | Содержимое файла |
|---|---|---|
| `--runner vm` | Путь ВМ, `Linux` | `hello from Mac` |
| `--runner local` | Путь внутри Mac RunnerRoot, `Darwin` | `hello from Mac` |

Строка `[execution]` в stderr показывает checkout, фактический cwd, runner и результат синхронизации. Для Mac исполнитель обозначается `mac`, а синхронизация — `completed`.

Сохраняй одинарные кавычки вокруг `sh -c`: `$PWD` должен вычисляться на выбранном исполнителе. Первый запуск может скачивать образ Alpine.

## 10. Проверить перенос изменений в копию для прогона

**Терминал B — ВМ:**

```bash
printf 'edited on VM\n' > hello.txt
stealthbox run --runner local -- cat hello.txt
```

**Ожидается:** `edited on VM`.

**Терминал A — Mac:**

```bash
cat ~/Projects/stealthbox-demo/hello.txt
```

**Ожидается:** по-прежнему `hello from Mac`. Локальные исходники автоматически не заменяются; для прогона обновляется отдельная копия в RunnerRoot.

## 11. Проверить возврат результата

**Терминал B — ВМ, из демопроекта:**

```bash
stealthbox run --runner local -- sh -c \
  'printf "report from Mac\n" > report.txt'

stealthbox fetch \
  --file report.txt \
  --output /tmp/stealthbox-demo-report.txt

cat /tmp/stealthbox-demo-report.txt
```

**Ожидается:** `report from Mac`. Файл создан на Mac и скачан на ВМ. Забирай его до следующей синхронизации; при повторной проверке выбери новый путь `--output`.

## 12. Запустить агентов обычными командами

**Терминал B — ВМ:**

```bash
cd ~/Projects
codex login --device-auth
codex
```

Страницу авторизации открывай на Mac и вводи код из терминала ВМ. Если Codex уже авторизован, повторный login не нужен.

Пример первого запроса:

> Найди stealthbox-demo. Покажи hello.txt. Выполни через stealthbox run команду uname -s сначала с runner vm, потом с runner local. Файлы пока не меняй.

**Ожидается:** агент видит проект на ВМ, а результаты команд различаются: `Linux` и `Darwin`.

В другом окне tmux можно запустить `claude` из конкретного репозитория. Пройди его обычный вход в аккаунт, если требуется. Выбор агента в конфиге не нужен.

## 13. Проверить несколько проектов

**Терминал B — ВМ:**

```bash
mkdir -p ~/Projects/stealthbox-demo-second
cd ~/Projects/stealthbox-demo-second
git init
printf 'second project\n' > hello.txt
cd ~/Projects

stealthbox run --path stealthbox-demo --runner vm -- cat hello.txt
stealthbox run --path stealthbox-demo-second --runner local -- cat hello.txt
stealthbox list --discover
```

**Ожидается:** `edited on VM`, затем `second project`; оба репозитория видны в списке. Из корня Projects путь прогона указывается через `--path`; из самого репозитория он определяется по cwd.

## 14. Прогнать Zadolbator через Docker на Mac

Для этого этапа нужны исходники и vendor Zadolbator на Mac, образ `zadolbator-app:latest` и стандартный RunnerRoot из шага 3.

**Терминал A — Mac:**

```bash
docker image inspect zadolbator-app:latest --format '{{.Os}}/{{.Architecture}}'
test -f ~/Projects/itfinance/zadolbator/vendor/autoload.php && echo 'vendor OK'
```

**Ожидается:** `linux/amd64` и `vendor OK`. Helper не собирает образ автоматически.

Перед импортом настрой исключения по [профильной инструкции Zadolbator](zadolbator-manual-test.md). В v0.4.3 `source_excludes` содержит буквальные пути, `.gitignore` не применяется. Переноси только `itfinance/zadolbator`; локальные конфиги исключаются, отслеживаемые шаблоны `environments` сохраняются. Большой список здесь не дублируется.

После изменения конфига, когда локальные прогоны закончены:

```bash
stealthbox setup
stealthbox bridge-start

stealthbox source import \
  --path itfinance/zadolbator \
  --plan /tmp/zadolbator-manual-import.json

less /tmp/zadolbator-manual-import.json
```

Просмотри план, выйди через `q`, затем:

```bash
stealthbox source apply --plan /tmp/zadolbator-manual-import.json
```

**Терминал B — ВМ:**

```bash
cd ~/Projects/itfinance/zadolbator
test -e .git || git init
stealthbox bridge-health
```

Переходи к прогонам только после успешной проверки моста. Запускай команды по очереди:

```bash
stealthbox run --runner local -- bash /absolute/path/to/stealthbox/tools/zadolbator-qa.sh prepare

stealthbox run --runner local -- bash /absolute/path/to/stealthbox/tools/zadolbator-qa.sh common

stealthbox run --runner local -- bash /absolute/path/to/stealthbox/tools/zadolbator-qa.sh api

stealthbox run --runner local -- bash /absolute/path/to/stealthbox/tools/zadolbator-qa.sh frontend

stealthbox run --runner local -- bash /absolute/path/to/stealthbox/tools/zadolbator-qa.sh backend
```

Путь `/absolute/path/to/stealthbox/tools/zadolbator-qa.sh` — файл на Mac. При другом расположении репозитория замени его во всех командах.

**Ожидается:** отдельный Compose-проект в RunnerRoot, тесты без подключения к рабочим базам, возврат вывода и кода завершения. Предыдущий успешный локальный SSH→Mac Docker прогон дал **1262 passed, 2 incomplete, 0 failures/errors**. Это ориентир для того набора исходников; incomplete не считай пройденными тестами.

## 15. Забрать отчёт и убрать тестовый стек

**Терминал B — ВМ, из Zadolbator:**

```bash
stealthbox fetch \
  --file .stealthbox-qa/reports/common.xml \
  --output /tmp/zadolbator-manual-common.xml

head /tmp/zadolbator-manual-common.xml

stealthbox run --runner local -- bash /absolute/path/to/stealthbox/tools/zadolbator-qa.sh status

stealthbox run --runner local -- bash /absolute/path/to/stealthbox/tools/zadolbator-qa.sh down
```

**Ожидается:** отчёт скачан на ВМ, тестовый стек удалён. Код, vendor и отчёты в RunnerRoot сохраняются. После прерванного прогона также выполни `down`.

Отсоединись от tmux своим префиксом и `d`. Если все локальные работы закончены, **на Mac**:

```bash
stealthbox bridge-stop
```

Остановка моста может прервать активные локальные команды, поэтому выполняй её после завершения прогонов. ВМ этот шаг не удаляет.

## Что сохранить при ошибке

Запиши номер шага, машину выполнения, полную команду, её вывод и код завершения. Код проверяй сразу после команды:

```bash
echo $?
```

Для проблемы связи проверь **на Mac**:

```bash
stealthbox bridge-status
stealthbox doctor
```

И **на ВМ**:

```bash
stealthbox bridge-health
```

Стандартный лог моста на Mac: `~/.config/stealthbox/mac.sock.log`. Если используется свой путь socket, лог находится рядом с ним с суффиксом `.log`. Не отправляй целиком конфиг с токеном моста или файлы авторизации агентов.

Документ — инструкция для будущего ручного прогона, а не подтверждение его прохождения. Последний временный облачный прогон v0.4.3 остановился на проверке удалённого моста; облачная цепочка тогда не была подтверждена полностью.
