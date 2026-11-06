# Core 1.1.15

Основа остаётся опубликованным Xray v26.10.31.

- В Linux отображаемое имя процесса изменено на `rc-core`. Имя исполняемого файла `rw-core`, аргументы запуска, пути и контракты core-manager сохраняются. Если отображаемое имя изменить нельзя, запуск ядра продолжается.
- Сохраняются исправления MUX, Xera, uTLS и offline recovery из 1.1.14. Сроки доступа и квоты не продлеваются.

Пересоберите схему и WASM редактора из Core 1.1.15 и поставляйте их вместе с панелью. Образ Node 1.1.24 включает это ядро; обновление удалённой ноды выполняется отдельно от панели. Замена процесса прерывает его соединения.

---

The base remains published Xray v26.10.31.

- Changes the Linux process display name to `rc-core`. The `rw-core` executable filename, launch arguments, paths and core-manager contracts remain compatible. Failure to change the display name does not prevent Core from starting.
- Retains the MUX, Xera, uTLS and offline recovery fixes from 1.1.14. Access expiry and quotas are not extended.

Rebuild the editor schema and WASM from Core 1.1.15 and ship them together with the panel. The Node 1.1.24 image bundles this Core; remote nodes are updated separately from the panel. Replacing the process interrupts its connections.
