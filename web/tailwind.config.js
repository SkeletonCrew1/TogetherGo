/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        brand: {
          50: '#eef6ff',
          100: '#d9eaff',
          200: '#bcdaff',
          300: '#8ec2ff',
          400: '#589fff',
          500: '#317bfb',
          600: '#1a5cf0',
          700: '#1548dc',
          800: '#183db2',
          900: '#19378c',
        },
      },
      keyframes: {
        'fade-in': { from: { opacity: '0' }, to: { opacity: '1' } },
      },
      animation: { 'fade-in': 'fade-in 150ms ease-out' },
    },
  },
  plugins: [],
}
