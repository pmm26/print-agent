import '@testing-library/jest-dom/vitest'
import { afterAll, afterEach, beforeAll } from 'vitest'
import { cleanup } from '@testing-library/react'
import { server } from './server'

Object.defineProperty(window, 'matchMedia', {
  writable: true,
  value: (query: string) => ({ matches: false, media: query, onchange: null, addListener: () => undefined, removeListener: () => undefined, addEventListener: () => undefined, removeEventListener: () => undefined, dispatchEvent: () => false }),
})

class ResizeObserverMock { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(window, 'ResizeObserver', { value: ResizeObserverMock })
Object.defineProperty(Element.prototype, 'hasPointerCapture', { value: () => false })
Object.defineProperty(Element.prototype, 'setPointerCapture', { value: () => undefined })
Object.defineProperty(Element.prototype, 'releasePointerCapture', { value: () => undefined })

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => { cleanup(); server.resetHandlers() })
afterAll(() => server.close())
