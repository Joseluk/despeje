# Contribuir a despeje

¡Gracias por querer echar una mano! Cualquier ayuda cuenta: avisar de un fallo,
mejorar la documentación, probar con tu operador o enviar código.

## Avisar de un problema

Abre un [issue](https://github.com/Joseluk/despeje/issues/new/choose) con la
plantilla que encaje. Lo más útil es la salida de:

```powershell
& $env:USERPROFILE\.despeje\despeje.exe -check
```

No publiques contraseñas, claves privadas ni nada que no quieras que sea público.
Las vulnerabilidades, mejor [en privado](https://github.com/Joseluk/despeje/security/advisories/new).

## Enviar código

1. Haz un fork y una rama para tu cambio.
2. Necesitas Go (la versión de `go.mod`). Los tests incluyen un servidor SSH en
   memoria que hace de VPS, así que no necesitas uno para desarrollar:

   ```powershell
   go vet ./...
   go test ./...
   ```

3. Si tocas el instalador (`install.ps1`) o el túnel, pruébalo en Windows con
   un VPS de verdad:

   ```powershell
   .\build.ps1
   $env:DESPEJE_LOCAL_DIST = "$PWD\dist"; .\install.ps1
   ```

4. Abre un pull request explicando qué cambia y cómo lo has probado.

### Algunas normas de la casa

- **Los `.ps1` en ASCII y sin BOM**: Windows PowerShell 5.1 rompe las tildes y
  `irm | iex` no se lleva bien con el BOM.
- **El proceso en segundo plano no debe tocar la configuración del sistema**:
  es lo que hace saltar a los antivirus. Los cambios en el sistema van en el
  instalador, a la vista y anunciados.
- **Nada de telemetría** ni conexiones a servidores que el usuario no haya
  configurado (ver la política de privacidad en el README).
- Mensajes, comentarios y documentación en español; el README en inglés se
  mantiene en paralelo.

## Ideas por las que empezar

- Soporte para macOS y Linux (el proxy ya es multiplataforma; falta configurar
  el proxy del sistema y el arranque).
- Una página de estado en HTML en `127.0.0.1:9119`.
- Traducciones del README.
