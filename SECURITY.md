# Seguridad

Si encuentras una vulnerabilidad en despeje (en el programa, el instalador, el
script del VPS o la cadena de publicación), **no abras un issue público**.
Infórmala en privado desde
[Security → Report a vulnerability](https://github.com/Joseluk/despeje/security/advisories/new).

Te responderemos lo antes posible y, cuando esté corregida, publicaremos un
aviso dándote el crédito si quieres.

## Qué cubre

- Ejecución de código o escalada de privilegios a través del instalador o de
  `setup-vps.sh`.
- Formas de que el usuario `despeje` del VPS haga algo más que abrir túneles.
- Fallos en la verificación de la huella del VPS, de los checksums o de la firma.
- Que el proxy local acepte conexiones de fuera del propio equipo.

## Versiones soportadas

Solo la [última release](https://github.com/Joseluk/despeje/releases/latest).
Para actualizar, vuelve a ejecutar el instalador.
