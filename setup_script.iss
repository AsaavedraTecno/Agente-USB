; Script de Instalación - Agente USB TecnoData
; ---------------------------------------------
; Compilar el ejecutable antes de correr esto: go build -ldflags="-H=windowsgui" -o AgenteUSB.exe .
; -H=windowsgui: sin esto, cada arranque (GUI, "--tray", el servicio, --install/--uninstall)
; abre/parpadea una consola cmd negra detrás — el binario queda marcado subsistema GUI, sin
; consola en ninguno de esos modos. No afecta a --headless/--bruteforce/--httpprobe corridos
; a mano desde una terminal ya abierta (heredan la consola existente igual).

#define MyAppName "TecnoData Agente USB"
; /DMyAppVersion=X.Y.Z la sobreescribe desde afuera para que coincida con la
; versión del binario — el #ifndef es necesario porque un #define normal acá
; abajo pisaría incondicionalmente lo que haya llegado por /D.
#ifndef MyAppVersion
  #define MyAppVersion "1.1.0"
#endif
#define MyAppPublisher "TecnoData"
#define MyAppExeName "AgenteUSB.exe"

[Setup]
AppId={{7C1E9F3A-4B2D-4E8C-9A6F-3D5B8E2C1A70}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
DefaultDirName={autopf}\{#MyAppName}
OutputBaseFilename=Instalador_Agente_USB_TecnoData
Compression=lzma
SolidCompression=yes
PrivilegesRequired=admin
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayIcon={app}\{#MyAppExeName}

[Languages]
Name: "spanish"; MessagesFile: "compiler:Languages\Spanish.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked

[Files]
Source: "AgenteUSB.exe"; DestDir: "{app}"; Flags: ignoreversion

[Dirs]
; Carpeta compartida para logs, cola y estado del servicio (winsvc.Install
; también la crea, pero dejarla lista aquí evita una carrera si algo la lee
; antes del primer --install).
Name: "{commonappdata}\AgenteUSB"; Permissions: users-modify

[Icons]
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Tasks: desktopicon
; El servicio ya arranca solo con Windows (StartType: Automatic, ver
; winsvc.Install), pero eso no muestra nada — sin este acceso directo,
; después de reiniciar el PC no queda ningún ícono visible que confirme que
; el agente sigue vivo, hasta que alguien abra la app a mano. "--tray"
; arranca solo el ícono de bandeja, sin ventana, para no interrumpir el
; inicio de sesión.
; {commonstartup} (todos los usuarios), no {userstartup}: el instalador
; corre con PrivilegesRequired=admin, y si IT lo instala con una cuenta
; distinta de quien usa la PC a diario, {userstartup} habría quedado en el
; perfil del admin, no del usuario real — nunca se habría abierto solo.
Name: "{commonstartup}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Parameters: "--tray"

[Run]
; --install ya crea el servicio Y lo arranca (a diferencia de AgentSNMP, que
; necesita "install" + "start" por separado) — ver winsvc.Install().
Filename: "{app}\{#MyAppExeName}"; Parameters: "--install"; Flags: runhidden waituntilterminated
; Abre la GUI para que el usuario configure la API Key / cliente
Filename: "{app}\{#MyAppExeName}"; Description: "Configurar Agente Ahora"; Flags: nowait postinstall skipifsilent

[UninstallRun]
Filename: "{app}\{#MyAppExeName}"; Parameters: "--uninstall"; RunOnceId: "UninstallService"; Flags: runhidden waituntilterminated

[UninstallDelete]
; Borra config, logs, cola, estado y el machine_id (identidad del agente) al
; desinstalar. Es intencional: una reinstalación limpia se trata como una
; máquina distinta para efectos de monitoreo (ver internal/identity).
Type: filesandordirs; Name: "{commonappdata}\AgenteUSB"
