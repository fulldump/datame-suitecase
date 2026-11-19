# Datame Suitcase

Guarda en tu NAS de [Datame](https://github.com/fulldump/datame) una copia de lo que tienes en otros servicios: tu blog, tu archivo de X (Twitter), tus chats de Telegram, tus repositorios de GitHub… Cada cierto tiempo trae lo nuevo y lo escribe en una carpeta del NAS, en ficheros normales (JSON, Markdown, HTML y las fotos y vídeos originales) que se podrán leer dentro de 50 años sin este programa.

**Estado: primera versión, en desarrollo.** Funciona, pero no le confíes aún lo único que tienes de algo.

## Cómo funciona

```
 blog, X, Telegram,        ┌──────────────────┐   HTTPS con la clave    ┌───────────┐
 GitHub, una carpeta… ───► │  datame-suitcase │ ──── del NAS fijada ──► │ NAS Datame│
 (API, feed o exportación) │  (web local)     │   token de dispositivo  │  /Archivo │
                           └──────────────────┘                         └───────────┘
```

- Corre en segundo plano, en el propio NAS o en un PC de casa, con una web pequeña en `http://127.0.0.1:4141`.
- Escribe en el NAS **igual que la app del móvil**: con un código de emparejamiento que creas en la web del NAS. Ese código solo deja listar, leer y **añadir** ficheros: no puede borrar nada.
- Cada servicio se configura eligiendo: el servicio, sus credenciales o su exportación, el volumen y la carpeta del NAS, y cada cuánto se copia.
- **Nunca duplica.** Los nombres de los ficheros dependen solo del contenido (el id de un tuit, la fecha y el enlace de una entrada), no de cuándo se ejecutó. Cada fichero se envía con «solo si no existe» (`base=none`); si ya existe, se comprueba si es igual antes de hacer nada. Lo que puede cambiar (una entrada editada, un mes con más mensajes) se reemplaza, y el NAS guarda la versión anterior en su historial.
- **Solo trae lo nuevo.** Cada servicio guarda dónde se quedó (un cursor): el feed responde «no ha cambiado», una exportación ya importada no se vuelve a leer, GitHub solo da las issues actualizadas desde la última vez. Si una copia se corta a medias, la siguiente sigue por donde iba.
- **Suma, no resta.** Lo que borras en el servicio sigue en la copia: un tuit borrado en X sigue en el mes del NAS aunque ya no venga en el archivo nuevo.

## Instalar

Hace falta Go 1.27 para compilar:

```sh
go build -o datame-suitcase ./cmd/datame-suitcase
```

Para copiar el código de GitHub hace falta además `git` en esa máquina.

### En tu PC

Como servicio de tu usuario, para que lea tus Descargas y arranque solo:

```sh
mkdir -p ~/.local/bin ~/.config/systemd/user
cp datame-suitcase ~/.local/bin/
cp deploy/datame-suitcase-user.service ~/.config/systemd/user/datame-suitcase.service
systemctl --user daemon-reload
systemctl --user enable --now datame-suitcase
loginctl enable-linger "$USER"   # para que siga copiando aunque cierres la sesión
```

Abre <http://127.0.0.1:4141>. Los datos quedan en `~/.local/share/datame-suitcase`.

### En el NAS

Como servicio del sistema, con un usuario propio:

```sh
sudo cp datame-suitcase /usr/local/bin/
sudo useradd --system --home /var/lib/datame-suitcase --shell /usr/sbin/nologin datame-suitcase
sudo cp deploy/datame-suitcase.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now datame-suitcase
```

Los datos quedan en `/var/lib/datame-suitcase`. Las exportaciones que importes (ZIP de X, carpetas de Telegram) deben estar donde ese usuario pueda leerlas, por ejemplo `/srv/datame-suitcase/entrada` (descomenta `ReadOnlyPaths` en el servicio).

La web solo responde a `localhost`. Para abrirla desde tu PC, lo más seguro es un túnel SSH:

```sh
ssh -L 4141:127.0.0.1:4141 usuario@nas
```

y abrir <http://127.0.0.1:4141> en tu PC. Si prefieres abrirla directamente por la red, arranca con `-listen 0.0.0.0:4141 -password 'algo largo'`: pedirá la contraseña, pero va por HTTP sin cifrar, así que cualquiera en tu red podría verla pasar.

### Opciones

```
-dir        carpeta de datos (por defecto ~/.local/share/datame-suitcase, o la de systemd)
-keyfile    fichero de la clave de las credenciales (por defecto <dir>/clave)
-listen     dirección de la web (por defecto 127.0.0.1:4141)
-password   contraseña de la web; obligatoria si escucha fuera de localhost
```

También van en un `config.json` (`-config`) o en variables de entorno con el prefijo `SUITCASE_`: `SUITCASE_DIR`, `SUITCASE_LISTEN`, `SUITCASE_PASSWORD`, `SUITCASE_KEYFILE` (la ayuda de `-help` las muestra sin el prefijo).

## Emparejar con el NAS

1. En la web del NAS, ve a **Conexiones → Móviles → Añadir**. Ponle un nombre que lo distinga, por ejemplo «Suitcase», y elige el volumen donde guardará. Entra en la web del NAS por su IP de la red (no por `localhost`): el código lleva dentro las direcciones por las que se le busca.
2. Copia el código, que empieza por `datame-movil:`.
3. En la web de Suitcase, pulsa **Emparejar con el NAS** y pégalo. Comprueba que el NAS responde antes de guardarlo.

El código lleva las direcciones del NAS, la huella de su clave y un token. Suitcase reconoce al NAS por la huella (SHA-256 de su clave pública), no por su nombre ni su IP: si en esa dirección responde otro equipo, se niega a hablar con él. Para retirarle el permiso, borra el dispositivo en la web del NAS; Suitcase lo dirá en cuanto lo intente («vuelve a emparejarlo»).

El NAS sugiere la carpeta `Móvil/<nombre>` para los móviles; Suitcase no la usa: cada servicio elige la suya (`/Archivo/Blog`, `/Archivo/Twitter`…), y se puede cambiar.

## Servicios

### Blog (RSS o Atom)

Pega la dirección del feed: suele ser `/feed` (WordPress), `/rss.xml`, `/atom.xml` o `/index.xml` (Hugo). Por cada entrada guarda, en `AAAA/AAAA-MM-DD-<nombre>/`:

- `index.html`: una página que se abre sola en cualquier navegador, con las imágenes enlazadas a la copia local.
- `index.md`: el texto, con título, fecha, autor y enlace al original.
- `index.json`: todo lo que dice el feed, con el HTML original.
- `media/`: las imágenes de la entrada.

Los feeds solo traen las últimas entradas (10 a 50), así que **conviene darlo de alta cuanto antes**: desde entonces no se perderá ninguna, pero lo anterior hay que traerlo de otro modo (la exportación de WordPress, por ejemplo, con «Carpeta local»). Los adjuntos de los podcasts se guardan como enlace, no se descargan.

Sirve también para cualquier cosa con feed: un perfil de Mastodon (`https://instancia/@usuario.rss`), un canal de YouTube (`https://www.youtube.com/feeds/videos.xml?channel_id=…`, solo título y descripción de los últimos vídeos), Substack, Medium…

### X / Twitter

La API de X es de pago, así que se usa el archivo que X da gratis a cada cuenta:

1. En X: **Más → Configuración y privacidad → Tu cuenta → Descargar un archivo de tus datos**. Pide la contraseña y un código.
2. Tarda de uno a varios días. Llega un aviso, y el enlace caduca en unos días.
3. Deja el ZIP (`twitter-AAAA-MM-DD-….zip`) en una carpeta, y pon esa carpeta en Suitcase. Cada vez que dejes uno nuevo, se importará lo que falte. Sirve también un ZIP concreto o el archivo ya descomprimido.

Guarda:

- `tweets/AAAA/AAAA-MM.json`: los tuits de cada mes, tal como vienen en el archivo.
- `tweets/AAAA/AAAA-MM.md`: los mismos, para leer, con los enlaces `t.co` cambiados por los de verdad y las fotos.
- `media/AAAA/AAAA-MM/`: fotos y vídeos, con el nombre que trae el archivo (`<id del tuit>-<nombre>`).
- `me-gusta.json` y `me-gusta.md`.
- `originales/`: el ZIP tal como llegó, que tiene además los mensajes directos, los seguidores y todo lo demás. Se puede desactivar.

### Telegram

Telegram solo exporta desde el programa de ordenador (Telegram Desktop):

1. **Ajustes → Avanzado → Exportar datos de Telegram**.
2. Elige los chats y los tipos de fichero (fotos, vídeos, audios, ficheros), y el formato **«JSON legible por máquinas»**.
3. Indica en Suitcase la carpeta que crea (`DataExport_AAAA-MM-DD`), un ZIP de ella, o la carpeta donde vas dejando las exportaciones.

Guarda cada chat en `chats/<id>-<nombre>/`: `AAAA/AAAA-MM.json` y `AAAA/AAAA-MM.md` por mes, y `media/AAAA-MM/<id del mensaje>-<nombre>`. La carpeta de un chat no cambia aunque le cambies el nombre. Los ficheros se nombran por el mensaje porque Telegram numera las fotos de forma distinta en cada exportación. También guarda `perfil.json` y `contactos.json` si los exportas.

### Google Takeout

Google da gratis una copia de todo lo de tu cuenta en <https://takeout.google.com>:

1. Elige qué exportar (por defecto, todo). En el paso siguiente: **«Exportar cada 2 meses durante 1 año»**, tipo **.zip** y tamaño de **50 GB**. Así llega sola una exportación nueva cada dos meses.
2. Cuando llegue el correo, descarga todas las partes (`takeout-…-001.zip`, `-002.zip`…) y déjalas juntas en una carpeta. Pon esa carpeta en Suitcase. Sirven también los `.tgz` y la carpeta ya descomprimida.
3. Puedes elegir qué productos importar. Las carpetas se reconocen en inglés y en español («Google Fotos» o «Google Photos», «Correo» o «Mail»…); las que no conoce van a `Otros/` tal cual, para que no se pierda nada.

Guarda:

- `Fotos/AAAA/AAAA-MM/`: cada foto y vídeo **una sola vez** (aunque esté en varios álbumes o partes), con la fecha en que se hizo y, al lado, el JSON de Google (fecha, lugar, descripción). `Fotos/Albumes/<álbum>/album.md` y `album.json` son la lista de cada álbum, con enlaces a esas fotos.
- `Correo/mbox/`: el correo tal como viene (se puede desactivar) y además **un `.eml` por correo** en `Correo/<etiqueta>/AAAA/AAAA-MM/`, que abre cualquier programa de correo. Un correo se guarda una vez, por su `Message-ID`, aunque luego cambie de etiqueta.
- `Keep/`: cada nota en JSON, en HTML y en Markdown (título, texto, listas, etiquetas y fechas), con sus adjuntos.
- `Calendario/` y `Contactos/`: los `.ics` y `.vcf`, con un resumen en Markdown al lado.
- `Drive/`, `YouTube/` (tus vídeos, historial y listas), `Maps/`, `Chrome/`: tal como vienen.
- `Ubicaciones/`: el historial tal como viene y, si trae `Records.json`, un CSV por año (`resumen/AAAA.csv`).
- `Originales/`: los ZIP, solo si lo activas: ocupan mucho y ya está todo guardado por productos.

Cada exportación es la cuenta entera, pero solo se escribe lo nuevo o lo que cambió; lo que borres en Google sigue en el NAS. Una parte que no se puede abrir (porque aún se descarga) o que falta en la numeración hace esperar a esa exportación hasta la siguiente copia. Los ficheros se leen como un flujo, sin cargarlos en memoria: sirven partes de 50 GB. Los `.tgz` se leen dos veces, porque no se puede saltar dentro de ellos, y cada fichero grande se copia un momento a la carpeta temporal.

### GitHub

1. Crea un token en **GitHub → Settings → Developer settings → Personal access tokens → Fine-grained tokens**, con acceso a tus repositorios y permisos de **solo lectura**: Contents, Issues, Pull requests y Metadata.
2. En Suitcase pon tu usuario y el token. Sin token solo se copian los repositorios públicos, y GitHub permite 60 peticiones por hora.

Guarda, por cada repositorio, en `repos/<nombre>/`:

- `<nombre>.bundle`: todo el repositorio (ramas y etiquetas) en un fichero. Se recupera sin GitHub ni Suitcase con `git clone proyecto.bundle proyecto`. Se rehace solo cuando cambia alguna rama o etiqueta; el NAS guarda los anteriores.
- `repo.json`: lo que dice GitHub del repositorio.
- `issues/NNNNN.json` y `.md`: issues y pull requests con sus comentarios.

Y `estrellas.json` / `estrellas.md`. Los forks se pueden incluir. El token se pasa a `git` por variables de entorno, no en la línea de órdenes, y solo a `github.com` (o al servidor de la API configurada).

Cada bundle es el repositorio entero: un repositorio grande que cambia a menudo ocupa una copia entera por cada cambio en el historial del NAS. Ver «Pendiente».

### Correo (IMAP)

Copia el correo de Gmail, Outlook/Hotmail, iCloud, Fastmail, Yahoo o cualquier proveedor con IMAP. Hace falta una **contraseña de aplicación** (casi siempre con la verificación en dos pasos activada), no la normal de la cuenta:

- **Gmail:** <https://myaccount.google.com/apppasswords>, ponle un nombre y copia las 16 letras.
- **Outlook / Hotmail:** account.microsoft.com → Seguridad → Opciones de seguridad avanzadas → Contraseñas de aplicación. Microsoft está retirando este acceso en muchas cuentas; si no la acepta, puede que la tuya ya no lo permita.
- **iCloud:** account.apple.com → Inicio de sesión y seguridad → Contraseñas específicas para apps. El usuario es la dirección `@icloud.com`.

En «Servidor» basta con escribir `gmail`, `outlook`, `icloud`, `fastmail` o `yahoo`, o dejarlo vacío para que lo deduzca de la dirección. Va siempre cifrado (TLS en el puerto 993, o STARTTLS en el 143 si se marca); si el servidor no permite cifrar, no se envía la contraseña. **No cambia nada en el buzón**: abre las carpetas en solo lectura y no marca nada como leído.

Guarda, en `<carpeta>/AAAA/AAAA-MM/`:

- `AAAA-MM-DD-<asunto>-<huella>.eml`: cada correo tal como está en el servidor, con sus adjuntos dentro. Lo abre cualquier programa de correo, y es texto. La huella sale del propio contenido, así que un correo nunca se guarda dos veces.
- `indice.json` e `indice.md`: fecha, remitente, asunto y fichero de cada correo del mes.
- Opcionalmente, los adjuntos también sueltos, en una carpeta con el nombre del correo.

Por cada carpeta recuerda el último correo copiado (`UID` y `UIDVALIDITY`): solo trae los nuevos. Si el servidor renumera una carpeta, la repasa reconociendo por su `Message-ID` lo ya copiado, sin volver a descargarlo. Copia como mucho 5.000 correos por ejecución (se puede cambiar): un buzón grande se completa en varias, cada una sigue donde lo dejó la anterior. La papelera y el correo no deseado se saltan por defecto.

**Gmail** muestra cada etiqueta como una carpeta, así que copiarlas todas repetiría los correos. Se copia solo «Todos» (`[Gmail]/All Mail`), que tiene cada correo una vez, y sus etiquetas se apuntan en el índice. Si escribes carpetas concretas, se copian esas.

### Calendarios y contactos (CalDAV y CardDAV)

Para iCloud, Nextcloud, Fastmail, Radicale, Baïkal y cualquier servidor CalDAV o CardDAV. Se pone el servidor (`icloud`, `fastmail` o su dirección, por ejemplo `https://tu-servidor/remote.php/dav` en Nextcloud), el usuario y una **contraseña de aplicación** (iCloud: appleid.apple.com → Contraseñas específicas de apps; Fastmail y Nextcloud: en sus ajustes de seguridad). Los calendarios y las libretas se buscan solos (`/.well-known/caldav`), y se puede copiar solo una de las dos cosas.

Guarda cada calendario en `Calendarios/<nombre>/` y cada libreta en `Contactos/<nombre>/`:

- `eventos/*.ics` y `contactos/*.vcf`: cada evento y contacto tal como lo tiene el servidor (con su foto dentro). Es el formato de siempre de los calendarios y las agendas.
- `<nombre>.ics` / `<nombre>.vcf`: todos juntos, para importarlos de una vez en cualquier aplicación. Es el calendario tal como está hoy en el servidor.
- `<nombre>.md`: para leer: los eventos por año y mes (fecha, título, lugar, si se repite) y los contactos con sus teléfonos y correos.
- `indice.json`: qué fichero es cada elemento del servidor. Con él, una instalación nueva sigue donde lo dejó la anterior.

Solo pide lo que cambió: con el token de `sync-collection` (RFC 6578) si el servidor lo tiene, o comparando las ETag si no, y trae los cambios en lotes. Lo que borras en el servidor sigue en el NAS, tachado en el resumen y con el día en que se vio borrado.

**Google no funciona todavía:** su CalDAV y CardDAV solo dejan entrar con OAuth, no con contraseñas de aplicación. Para Google Calendar usa la «dirección secreta en formato iCal» de cada calendario con «Fichero de una dirección web», y para los contactos, Takeout. **Outlook** no tiene CalDAV: hace falta un intermediario como DavMail.

### Carpeta local

Copia una carpeta de la máquina donde corre Suitcase: lo nuevo y lo que cambia (el NAS guarda la versión anterior). Lo que borres en la carpeta sigue en el NAS. Sirve para cualquier exportación que no tenga su propio servicio todavía: Google Takeout descomprimido, el ZIP de LinkedIn, la exportación de WordPress o de Instagram.

### Fichero de una dirección web

Descarga un fichero cada vez que cambia: un calendario `.ics` privado, una exportación publicada. La dirección se guarda cifrada, porque a menudo lleva una clave. Puede reemplazar el fichero (el NAS guarda las versiones) o guardar uno con fecha cada vez.

## Las credenciales

Los tokens (el del NAS, el de GitHub, las direcciones privadas) se guardan **cifrados con AES-256-GCM**. La clave está en un fichero aparte, `clave`, que solo puede leer el usuario que ejecuta el programa (permisos `0600`).

Esto protege las credenciales si la carpeta de datos se copia, se sincroniza o acaba en una copia de seguridad sin el fichero de la clave. **No las protege de quien pueda leer ficheros como ese usuario**, porque el programa tiene que poder arrancar solo, sin que nadie escriba una contraseña. Es la misma decisión que el desbloqueo automático del NAS. Para separar más, pon la clave en otro disco con `-keyfile`.

Se descartó el llavero del sistema (Secret Service, Keychain) porque en un NAS sin escritorio no hay ninguno abierto, y sería un comportamiento distinto en cada máquina. Si se pierde la clave, las credenciales no se pueden leer: el programa lo dice al arrancar, y hay que volver a emparejar y escribir los tokens. Lo ya copiado en el NAS no depende de esto.

La web no muestra nunca un token guardado: al editar, un campo secreto vacío conserva el valor anterior.

## Otros servicios: cómo se harían

Estos no están en esta versión. Lo que se puede hacer ya, y el plan:

### LinkedIn

LinkedIn no da API para que una persona lea sus propias publicaciones o mensajes: las APIs de miembros (Community Management, Marketing) exigen ser socio y pasar una revisión, y «Sign In with LinkedIn» solo da el nombre y el correo.

**Hoy:** **Configuración → Privacidad de datos → Obtener una copia de tus datos**. La parte rápida llega en unos minutos y el archivo completo en unas 24 horas: un ZIP con CSV (`Connections.csv`, `messages.csv`, `Profile.csv`, `Shares.csv`, `Positions.csv`…). Descomprímelo en una carpeta y usa «Carpeta local».

**Plan:** un importador como el de X: cada CSV a JSON y Markdown (las publicaciones por meses, los mensajes por conversación), fusionando con lo que ya hay. No trae las imágenes de las publicaciones, solo enlaces.

### Google (Gmail, Drive, Fotos, Calendar) y Google Workspace

Hay tres caminos, y ninguno es perfecto:

1. **Google Takeout** (<https://takeout.google.com>). Exporta todo: Gmail en `mbox`, Drive con los documentos convertidos a Office, Fotos con un JSON por foto, Calendar en `.ics`, YouTube… Se puede programar **cada 2 meses durante un año**, en ZIP de hasta 50 GB. Lo malo: los enlaces de descarga exigen tu sesión de Google, así que alguien tiene que descargarlos (o pedir que lo deje en Drive, y entonces hace falta la API de Drive). **Hoy:** descarga, descomprime y usa «Carpeta local». **Plan:** un importador de Takeout que reconozca cada producto (separar el `mbox` en un `.eml` por correo, fusionar Fotos con sus JSON).
2. **APIs con OAuth.** Gmail y Drive tienen APIs incrementales muy buenas (`history` de Gmail, `changes` de Drive). Pero sus permisos de lectura (`gmail.readonly`, `drive.readonly`) son «restringidos»: una app publicada para todo el mundo necesita la verificación de Google y una auditoría de seguridad anual de pago. Para un programa libre que cada uno instala en casa, la salida es que **cada persona registre su propia app** en Google Cloud Console (gratis): con una cuenta normal, la app queda «en pruebas» y Google caduca el permiso **cada 7 días**, así que habría que volver a autorizar cada semana; con **Google Workspace** (la de pago, la antigua GSuite) la app puede ser «interna» a la organización, sin verificación ni caducidad. La autorización se haría con el flujo de app instalada, redirigiendo a la propia web local (`http://127.0.0.1:4141/oauth/…`). El flujo de dispositivo (el de las teles) no sirve: no admite los permisos de Gmail.
3. **IMAP con contraseña de aplicación**, para el correo. Con la verificación en dos pasos activada, Google permite crear contraseñas de aplicación, y por IMAP se leen solo los correos nuevos (por `UID`). Guardaría un `.eml` por correo, el formato más duradero que hay. Funciona además con cualquier proveedor de correo. Un administrador de Workspace puede desactivarlo. **Es el siguiente servicio más rentable.**

**Plan:** primero IMAP genérico; después el importador de Takeout; las APIs, solo para cuentas de Workspace.

### YouTube

La API de YouTube (Data API v3) se usa con una clave gratuita sin revisión, con 10.000 unidades al día, y da los títulos, descripciones, listas y comentarios de un canal. **No permite descargar los vídeos**, y descargarlos por otros medios va contra las condiciones de YouTube.

**Hoy:** el feed del canal funciona con «Blog (RSS o Atom)», solo con los últimos 15 vídeos. Tus propios vídeos se descargan en YouTube Studio uno a uno, o todos con Takeout (producto «YouTube y YouTube Music»), junto a tus listas, comentarios e historial.

**Plan:** un servicio con la clave de la API para los metadatos de todo el canal (la lista de subidas cuesta 1 unidad por cada 50 vídeos), y los ficheros desde Takeout.

### Twitch

La API de Twitch (Helix) es gratuita: se registra una app en <https://dev.twitch.tv/console> sin revisión, y con sus credenciales se listan tus emisiones, clips y vídeos. No da los ficheros de vídeo. Las emisiones pasadas **caducan** a los pocos días (7 en la mayoría de cuentas; más con Partner, Turbo o Prime), así que lo urgente es descargarlas: Twitch lo permite desde el Productor de vídeo, a mano.

**Plan:** un servicio que guarde los metadatos de vídeos y clips con la API y avise de las emisiones que van a caducar. Descargar los vídeos por la lista HLS es posible pero no está documentado ni permitido, así que no se hará.

### Otros fáciles

- **Mastodon:** el feed ya funciona. El archivo completo (Preferencias → Importar y exportar → Solicitar tu archivo) es un `outbox.json` con los medios: un importador sencillo.
- **Bluesky:** la API pública da el repositorio entero de una cuenta, sin autenticación (`com.atproto.sync.getRepo`): muy factible.
- **Instagram y Facebook:** «Descargar tu información» en formato JSON; la API para leer tu propio contenido (Basic Display) se cerró en 2024. Importador como el de Telegram.

## Escribir un servicio nuevo

Cada servicio implementa `plugin.Plugin` (en `plugin/plugin.go`):

```go
type Plugin interface {
	Info() Info                                  // nombre, ayuda, campos del formulario, carpeta por defecto
	Validate(ctx context.Context, env Env) error // comprueba la configuración y, si es barato, las credenciales
	Sync(ctx context.Context, run *Run) error    // trae lo nuevo
}
```

En `Sync`, `run.Load(&cursor)` lee dónde se quedó, `run.Save(cursor)` lo guarda al momento, `run.Out.Write(ctx, plugin.File{…})` escribe un fichero en su carpeta del NAS (sin duplicar nunca), `run.Out.Read` lee uno ya escrito para fusionar, `run.Item(n)` cuenta los elementos nuevos y `run.Logf` escribe en el registro que muestra la web. Las reglas:

- Nombres estables: que dependan del contenido (un id, una fecha), nunca de la hora de la ejecución.
- Formatos que duren: JSON con lo que dio el servicio, Markdown o HTML para leer, los ficheros originales con su nombre.
- `Mutable: true` solo para lo que puede cambiar con razón; lo demás no se reemplaza nunca.
- Registrarlo en `plugins/all`.

Los tests usan `plugintest`, que lo ejecuta contra un NAS falso (`nastest`) con el escritor de verdad, y datos sintéticos generados en el propio test.

## Probar contra el NAS de demo

```sh
go build -o datame-suitcase ./cmd/datame-suitcase
./datame-suitcase -dir /tmp/suitcase-demo
```

1. Abre la web del NAS de demo por su IP, <https://192.168.0.73:4040>, ve a **Conexiones → Móviles → Añadir** y copia el código.
2. Abre <http://127.0.0.1:4141>, pulsa **Emparejar con el NAS** y pega el código.
3. Añade un servicio, por ejemplo «Blog (RSS o Atom)» con el feed de un blog, y pulsa **Ejecutar ahora**.
4. Mira en la web del NAS la carpeta `/Archivo/Blog`. Ejecútalo otra vez: no debe aparecer nada repetido.

## Detalles de la API del NAS

Lo que usa Suitcase, comprobado en `node/api.go` y `node/devices.go` de Datame:

| Petición | Uso |
|---|---|
| `GET /v0/me` | Comprueba el código al emparejar; volúmenes y si la máquina está activa |
| `GET /v0/file?volume=&path=` | Leer un fichero ya escrito, para comparar o fusionar |
| `PUT /v0/file?volume=&path=&mtime=&base=none` | Crear un fichero solo si no existe. Si existe: `412` con `{exists, version, size, mtime}` |
| `PUT /v0/file?volume=&path=&mtime=` | Reemplazar; el NAS guarda la versión anterior |
| `GET /v0/files?volume=` | Listar. **Devuelve el volumen entero e ignora `path`**, por eso Suitcase no lo usa para saber qué hay: lleva su propia lista de lo escrito y, si la pierde, pregunta fichero a fichero con `base=none` |

Respuestas: `401` token retirado, `403` sin acceso a ese volumen, `409` máquina en espera, `507` volumen lleno. En los tres últimos casos la copia se para y se reintenta en menos de una hora.

## Desarrollo

```sh
go vet ./...
go test -race ./...
```

Los tests no usan la red ni ficheros personales: el NAS, el blog, la API de GitHub y las exportaciones son falsos y se generan en cada test. El de GitHub necesita `git` y se salta si no está.

## Pendiente

- Probarlo contra un NAS real durante semanas.
- GitHub: bundles incrementales (`git bundle create … ^<lo ya enviado>`) para no guardar el repositorio entero en cada cambio; wikis y releases.
- Servicios: IMAP, importador de Google Takeout, LinkedIn, Mastodon, Bluesky (ver arriba).
- Leer las exportaciones muy grandes de Telegram sin cargar `result.json` entero en memoria.
- Avisar en la web cuando un servicio lleva días fallando.
- Que la lista de lo escrito no crezca sin límite en servicios con millones de ficheros.
- Cuando la web del NAS llame «Dispositivos» a «Móviles», actualizar estos textos.
