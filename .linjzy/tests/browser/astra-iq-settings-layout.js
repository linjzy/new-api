// Run through Playwright MCP browser_run_code_unsafe with the check settings
// dialog open and checks enabled. Edits are restored; settings are never saved.
async function astraIQSettingsLayout(page) {
  const dialog = page.getByRole('dialog')
  const start = dialog.locator('input[name="start_time"]')
  const end = dialog.locator('input[name="end_time"]')
  await start.waitFor({ state: 'visible' })
  const viewport = page.viewportSize()
  const original = [await start.inputValue(), await end.inputValue()]
  const results = []
  try {
    for (const width of [320, 375, 390, 430, 640, 768, 1280]) {
      await page.setViewportSize({ width, height: 844 })
      await start.fill('22:00')
      await end.fill('06:30')
      await dialog.evaluate((node) =>
        Promise.all(
          node
            .getAnimations({ subtree: true })
            .map((animation) => animation.finished)
        )
      )
      const layout = await dialog.evaluate((node) => {
        const rect = node.getBoundingClientRect()
        const viewportWidth = document.documentElement.clientWidth
        const controls = ['start_time', 'end_time'].map((name) => {
          const input = node.querySelector(`input[name="${name}"]`)
          const box = input.getBoundingClientRect()
          const field = input.closest('[data-slot="form-item"]')
          const fieldBox = field.getBoundingClientRect()
          const style = getComputedStyle(input)
          return {
            name,
            value: input.value,
            left: box.left,
            right: box.right,
            top: box.top,
            bottom: box.bottom,
            clientWidth: input.clientWidth,
            scrollWidth: input.scrollWidth,
            fieldLeft: fieldBox.left,
            fieldRight: fieldBox.right,
            appearance: style.appearance,
          }
        })
        return { left: rect.left, right: rect.right, viewportWidth, controls }
      })
      const [first, second] = layout.controls
      if (first.value !== '22:00' || second.value !== '06:30') {
        throw new Error(
          `${width}px: time fields cannot be edited independently`
        )
      }
      if (width < 640 && first.bottom > second.top) {
        throw new Error(
          `${width}px: mobile time controls must stack vertically`
        )
      }
      if (width >= 640 && first.right > second.left) {
        throw new Error(`${width}px: desktop time controls overlap`)
      }
      if (width >= 640 && Math.abs(first.top - second.top) > 1) {
        throw new Error(`${width}px: desktop time controls must share a row`)
      }
      for (const control of layout.controls) {
        // iOS native time controls can ignore border-box width with padding:
        // https://bugs.webkit.org/show_bug.cgi?id=301648
        if (control.appearance !== 'none') {
          throw new Error(
            `${width}px: native time appearance can override CSS sizing`
          )
        }
        if (
          control.left < 0 ||
          control.right > layout.viewportWidth ||
          control.left < layout.left ||
          control.right > layout.right ||
          control.left < control.fieldLeft ||
          control.right > control.fieldRight ||
          control.scrollWidth > control.clientWidth
        ) {
          throw new Error(`${width}px: ${control.name} overflows its container`)
        }
      }
      results.push({ width, ...layout })
    }
  } finally {
    await start.fill(original[0])
    await end.fill(original[1])
    if (viewport) await page.setViewportSize(viewport)
  }
  return results
}
