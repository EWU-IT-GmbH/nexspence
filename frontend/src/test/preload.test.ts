import { describe, expect, it } from 'vitest'

describe('jsdom Blob compatibility with native Response', () => {
  it('streams binary data without changing bytes', async () => {
    const bytes = new Uint8Array([0, 31, 139, 128, 255])
    const blob = new Blob([bytes], { type: 'application/gzip' })
    const response = new Response(blob)
    expect(response.headers.get('content-type')).toBe('application/gzip')
    expect(new Uint8Array(await response.arrayBuffer())).toEqual(bytes)
  })

  it('keeps FileReader support for uploaded files', async () => {
    const file = new File(['backup'], 'backup.tar.gz')
    const text = await new Promise<string>((resolve, reject) => {
      const reader = new FileReader()
      reader.onload = () => resolve(reader.result as string)
      reader.onerror = () => reject(reader.error)
      reader.readAsText(file)
    })
    expect(text).toBe('backup')
    expect(await new Response(file).text()).toBe('backup')
  })
})
