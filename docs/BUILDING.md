# Сборка ядра и редактора

Нужен Go 1.27.2. Из каталога `xray`:

```bash
go test ./infra/conf ./transport/internet/xerahttp ./app/dispatcher ./app/proxyman/command ./app/commander
CGO_ENABLED=0 go build -trimpath -o xray ./main
./xray version
```

Для редактора клонируйте [Remnacust-panel](https://github.com/lottman/Remnacust-panel) рядом и установите зависимости её frontend. Из корня ядра запустите `bash editor/go/build-remnacust.sh`. Для другого расположения панели задайте `REMNACUST_PANEL_SOURCE`. Скрипт обновит WASM, Go runtime для браузера и схему конфигурации.

После изменений ядра обновите исходный архив в соседнем [Remnacust-node](https://github.com/lottman/Remnacust-node): `python3 node/docker/package-remnacust-core.py` из корня ноды. Пересоберите ноду и панель, если изменили редактор.

[Выпуск компонентов](https://github.com/lottman/Remnacust-installer/blob/main/docs/PUBLISHING.md).

## Проверка конфигураций настоящих генераторов

Для Linux-проверки совместимости генераторов подготовьте JSON из соседних репозиториев с установленными зависимостями. Из общей родительской папки:

```bash
node Remnacust-panel/panel/backend/tests/profile-port-plan-fixture.cjs /tmp/compiled-port-plan.json
node Remnacust-node/node/tests/generate-api-config-fixture.cjs /tmp/node-api-lua-fixture.json
cd Remnacust-core/xray
REMNACUST_PORT_PLAN_FIXTURE=/tmp/compiled-port-plan.json \
REMNACUST_NODE_LUA_FIXTURE=/tmp/node-api-lua-fixture.json \
go test -race ./app/proxyman/command -run 'Test(PanelCompiledProfiles|NodeGeneratedLuaProfile)' -count=1
```

Эти тесты поднимают ядро с временными loopback-портами и Unix-сокетом, проверяют реальный TCP-трафик, служебную статистику, live-обновление и сохранение старого потока. Без соответствующей переменной тест явно пропускается; обычный запуск `go test` сам по себе не подтверждает совместимость с текущим генератором другого репозитория.
